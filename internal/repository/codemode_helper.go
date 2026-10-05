package repository

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"unicode/utf16"

	quickjs "github.com/buke/quickjs-go"
)

const (
	codemodeHelperArgument = "__wormtamer_review_codemode_helper"
	maxCodemodeFrame       = 8 << 20
	maxCodemodeCode        = 256 << 10
	maxCodemodeOutput      = 16 << 20
	maxCodemodeItems       = 100_000
	maxCodemodeConcurrent  = 8
)

//go:embed codemode_prelude.js
var codemodePrelude string

// Retain the upstream license in distributed binaries as well as source.
//
//go:embed pi-codemode.LICENSE
var codemodeLicense string

type codemodeMessage struct {
	Kind      string          `json:"kind"`
	OK        bool            `json:"ok"`
	ID        int64           `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Code      string          `json:"code,omitempty"`
	Text      string          `json:"text,omitempty"`
	Error     string          `json:"error,omitempty"`
	Response  json.RawMessage `json:"response,omitempty"`
}

func IsCodemodeHelperInvocation(arguments []string) bool {
	return len(arguments) == 1 && arguments[0] == codemodeHelperArgument
}

// RunCodemodeHelper runs only in the disposable, credential-free subprocess.
// The service, not this interpreter, executes and validates nested tool calls.
func RunCodemodeHelper(input io.Reader, output io.Writer) error {
	request, err := readCodemodeMessage(input)
	if err != nil || request.Kind != "run" || len(request.Code) > maxCodemodeCode {
		return errors.New("invalid codemode helper request")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	rt := quickjs.NewRuntime(quickjs.WithMemoryLimit(256<<20), quickjs.WithMaxStackSize(1<<20),
		quickjs.WithGCThreshold(16<<20), quickjs.WithModuleImport(false), quickjs.WithCanBlock(false))
	defer rt.Close()
	js := rt.NewBareContext()
	if js == nil {
		return errors.New("create codemode context")
	}
	defer js.Close()

	finished := false
	successPending := false
	var bridgeErr error
	outputBytes, outputItems := 0, 0
	finish := func(ok bool, message string) {
		if finished {
			return
		}
		finished = true
		if len(message) > MaxToolBytes {
			message = message[:utf8PrefixLength(message, MaxToolBytes)] + "[truncated]"
		}
		bridgeErr = writeCodemodeMessage(output, codemodeMessage{Kind: "done", OK: ok, Error: message})
	}
	rt.SetInterruptHandler(func() int {
		if finished || bridgeErr != nil {
			return 1
		}
		return 0
	})
	bridge := js.NewFunction(func(ctx *quickjs.Context, _ *quickjs.Value, args []*quickjs.Value) *quickjs.Value {
		if finished || bridgeErr != nil || len(args) < 2 {
			return ctx.NewUndefined()
		}
		switch args[0].ToString() {
		case "call":
			if len(args) != 4 {
				return ctx.ThrowTypeError("Invalid tool call")
			}
			message := codemodeMessage{Kind: "call", ID: args[1].ToInt64(), Name: args[2].ToString(), Arguments: json.RawMessage(args[3].ToString())}
			if len(message.Arguments) > maxCodemodeCode {
				finish(false, "Tool arguments exceed the 256 KiB limit")
				return ctx.ThrowRangeError("Tool arguments exceed the limit")
			}
			bridgeErr = writeCodemodeMessage(output, message)
		case "output":
			// ToString uses a NUL-terminated C string; this conversion retains its full length.
			units, err := args[1].ToStringUTF16()
			if err != nil {
				finish(false, err.Error())
				return ctx.NewUndefined()
			}
			text := string(utf16.Decode(units))
			outputItems++
			outputBytes += len(text) + 1
			if outputBytes > maxCodemodeOutput || outputItems > maxCodemodeItems {
				finish(false, "Script output exceeded 16 MiB or 100000 output items. Print less or write large data to a file with a tool.")
				return ctx.ThrowRangeError("Script output limit exceeded")
			}
			// Keep frames small even when text() prints a large string.
			for len(text) > 64<<10 && bridgeErr == nil {
				end := utf8PrefixLength(text, 64<<10)
				bridgeErr = writeCodemodeMessage(output, codemodeMessage{Kind: "output", Text: text[:end]})
				text = text[end:]
			}
			if bridgeErr == nil {
				bridgeErr = writeCodemodeMessage(output, codemodeMessage{Kind: "output", Text: text + "\n"})
			}
		case "done":
			if len(args) != 3 {
				return ctx.ThrowTypeError("Invalid script completion")
			}
			units, err := args[2].ToStringUTF16()
			if err != nil {
				finish(false, err.Error())
				return ctx.NewUndefined()
			}
			if args[1].ToBool() {
				successPending = true
			} else {
				finish(false, string(utf16.Decode(units)))
			}
		default:
			return ctx.ThrowTypeError("Unknown codemode bridge operation")
		}
		return ctx.NewUndefined()
	})
	defer bridge.Free()
	prelude := js.Eval("/*"+codemodeLicense+"*/\n"+codemodePrelude, quickjs.EvalFileName("codemode-prelude.js"))
	defer prelude.Free()
	if prelude.IsException() {
		return errors.New("initialize codemode prelude")
	}
	undefined := js.NewUndefined()
	defer undefined.Free()
	controller := prelude.Execute(undefined, bridge)
	defer controller.Free()
	if controller.IsException() {
		return errors.New("initialize codemode bridge")
	}
	code := js.NewString(request.Code)
	value := controller.Call("run", code)
	code.Free()
	value.Free()
	for !finished && bridgeErr == nil {
		if js.LoopOnce() == -2 && !finished {
			finish(false, scriptException(js))
			break
		}
		if finished || bridgeErr != nil {
			break
		}
		// LoopOnce drains ready microtasks. In the pinned binding, EvalAwait
		// checks the native unhandled-rejection tracker even for a non-promise;
		// evaluating void avoids waiting on intentionally unfinished tool calls.
		checkpoint := js.Eval("void 0", quickjs.EvalAwait(true))
		if checkpoint.IsException() {
			finish(false, "Unhandled promise rejection: "+scriptException(js))
		}
		checkpoint.Free()
		if finished {
			break
		}
		if successPending {
			finish(true, "")
			break
		}
		stalled := controller.Call("stalled")
		stalled.Free()
		if finished {
			break
		}
		response, readErr := readCodemodeMessage(input)
		if readErr != nil || response.Kind != "response" {
			return errors.New("read codemode tool response")
		}
		id := js.NewInt64(response.ID)
		payload := js.NewString(string(response.Response))
		settled := controller.Call("settle", id, payload)
		id.Free()
		payload.Free()
		if settled.IsException() {
			finish(false, scriptException(js))
		}
		settled.Free()
	}
	return bridgeErr
}

func scriptException(ctx *quickjs.Context) string {
	if err := ctx.Exception(); err != nil {
		return err.Error()
	}
	return "JavaScript execution failed"
}

func encodeCodemodeMessage(message codemodeMessage) ([]byte, error) {
	encoded, err := json.Marshal(message)
	if err != nil || len(encoded) > maxCodemodeFrame {
		return nil, errors.New("encode codemode frame")
	}
	frame := make([]byte, 4, len(encoded)+4)
	binary.BigEndian.PutUint32(frame, uint32(len(encoded)))
	return append(frame, encoded...), nil
}

func writeCodemodeMessage(output io.Writer, message codemodeMessage) error {
	frame, err := encodeCodemodeMessage(message)
	if err != nil {
		return err
	}
	_, err = output.Write(frame)
	return err
}

func readCodemodeMessage(input io.Reader) (codemodeMessage, error) {
	var header [4]byte
	if _, err := io.ReadFull(input, header[:]); err != nil {
		return codemodeMessage{}, err
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 || length > maxCodemodeFrame {
		return codemodeMessage{}, errors.New("invalid codemode frame length")
	}
	encoded := make([]byte, int(length))
	if _, err := io.ReadFull(input, encoded); err != nil {
		return codemodeMessage{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var message codemodeMessage
	if err := decoder.Decode(&message); err != nil {
		return message, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return message, errors.New("trailing codemode frame content")
	}
	return message, nil
}
