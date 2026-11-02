package repository

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCodemodeProtectsSharedBuiltins(t *testing.T) {
	workspace := testToolWorkspace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code := `
"use strict";
await tools.bash({command: "printf evidence > evidence.txt"});
// Keep both pending and queued calls alive while attempting mutations.
const reads = Array.from({length: 12}, () => tools.read({path: "evidence.txt"}));
const corrupt = () => { throw new Error("corrupted builtin called"); };
for (const [object, key] of [
    [Map.prototype, "get"],
    [Array.prototype, "push"],
    [Array.prototype, "toJSON"],
    [Object.prototype, "toJSON"],
    [Promise.prototype, "then"],
    [Function.prototype, "call"],
    [JSON, "parse"],
    [globalThis, "Promise"],
    [globalThis, "String"],
    [Object.getPrototypeOf([][Symbol.iterator]()), "next"],
]) {
    const original = object[key];
    try { object[key] = corrupt; } catch (error) {
        if (!(error instanceof TypeError)) throw error;
    }
    if (object[key] !== original) throw new Error("Changed builtin " + key);
}
const results = await Promise.allSettled([...reads, tools.read({path: "missing.txt"})]);
if (results.slice(0, -1).some(result => result.status !== "fulfilled" || result.value !== "evidence")) {
    throw new Error("Corrupted read responses");
}
if (results.at(-1).status !== "rejected" || !(results.at(-1).reason instanceof Error)) {
    throw new Error("Lost tool rejection");
}
const bash = await tools.bash({command: "printf after; exit 7"});
if (bash.output !== "after" || bash.exit_code !== 7) throw new Error("Corrupted Bash response");
text({reads: results.slice(0, -1).map(result => result.value), bash: bash.output});
return "builtins intact";`
	result, err := workspace.RunCode(ctx, map[string]any{"code": code}, codeTools(workspace))
	output, _ := result.Response["output"].(string)
	if err != nil || !strings.Contains(output, "builtins intact") ||
		strings.Count(output, `"evidence"`) != 12 || !strings.Contains(output, `"bash":"after"`) {
		t.Fatalf("builtin mutation = %+v, %v", result, err)
	}
}

func TestCodemodeBuiltinLockdownAllowsInstanceOverrides(t *testing.T) {
	workspace := testToolWorkspace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code := `
"use strict";
class EvidenceError extends Error {
    constructor(message) {
        super();
        this.name = "EvidenceError";
        this.message = message;
        this.code = "EVIDENCE";
    }
}
const object = {};
object.constructor = "own constructor";
object.hasOwnProperty = "own property";
object.toString = () => "own string";
object.valueOf = () => 42;
object.toJSON = () => ({value: +object});
const array = [];
array.push(object);
array.toString = () => "own array";
array.toJSON = () => ({items: array.length});
if (object.constructor !== "own constructor" || object.hasOwnProperty !== "own property" ||
    String(object) !== "own string" || String(array) !== "own array") {
    throw new Error("Instance overrides failed");
}
text({object, array});
const error = new EvidenceError("custom failure");
const [result] = await Promise.allSettled([Promise.reject(error)]);
text(result);
throw error;`
	result, err := workspace.RunCode(ctx, map[string]any{"code": code}, codeTools(workspace))
	if err != nil {
		t.Fatal(err)
	}
	output, _ := result.Response["error"].(string)
	for _, expected := range []string{
		`{"object":{"value":42},"array":{"items":1}}`,
		`"name":"EvidenceError"`, `"message":"custom failure"`, `"code":"EVIDENCE"`,
		"EvidenceError: custom failure",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("missing %q after instance overrides: %+v", expected, result)
		}
	}
}
