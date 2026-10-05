// Adapted from @earendil-works/pi-codemode 1.0.3, runtime/prelude-source.ts.
// Copyright (c) 2025 Mario Zechner. MIT license: see pi-codemode.LICENSE.
(function (bridge) {
    "use strict";
    const stringify = JSON.stringify;
    const parse = JSON.parse;
    const promiseThen = Promise.prototype.then;
    const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
    const ErrorCtor = Error;
    const pending = new Map();
    const queue = [];
    let nextId = 1;
    let finished = false;
    const EXIT = Object.freeze({});

    function errorText(error) {
        if (!(error instanceof ErrorCtor)) return String(error);
        const head = error.name + ": " + error.message;
        const frames = typeof error.stack === "string"
            ? error.stack.split("\n").filter(line => line.trim() && !line.includes("codemode-prelude.js"))
            : [];
        return [head, ...frames].join("\n");
    }

    function done(ok, error) {
        if (finished) return;
        finished = true;
        // The host checks unhandled rejections after draining ready microtasks
        // before it accepts a successful completion.
        bridge("done", ok, error);
    }

    function pump() {
        while (!finished && pending.size < 8 && queue.length > 0) {
            const entry = queue.shift();
            const id = nextId++;
            pending.set(id, entry);
            bridge("call", id, entry.name, entry.json);
        }
    }

    function caller(name) {
        return args => new Promise((resolve, reject) => {
            let json;
            // Serialization failures reject this promise without dispatching a tool call.
            try {
                json = stringify(args);
                if (json === undefined) throw new TypeError("Tool arguments must be an object");
            } catch (error) {
                reject(error);
                return;
            }
            queue.push({ name, json, resolve, reject });
            pump();
        });
    }

    function text(value) {
        if (finished) return;
        const rendered = value !== null && (typeof value === "object" || typeof value === "function")
            ? stringify(value) : String(value);
        bridge("output", rendered === undefined ? String(value) : rendered);
    }

    function exit() {
        done(true, "");
        throw EXIT;
    }

    const console = {};
    for (const level of ["log", "info", "warn", "error", "debug"]) {
        console[level] = (...args) => text(args.map(value => {
            if (value instanceof ErrorCtor) return errorText(value);
            if (typeof value === "string") return value;
            const json = stringify(value);
            return json === undefined ? String(value) : json;
        }).join(" "));
    }
    Object.freeze(console);
    const tools = Object.freeze({ read: caller("read"), bash: caller("bash") });
    for (const [name, value] of Object.entries({ tools, text, console, exit })) {
        Object.defineProperty(globalThis, name, { value, enumerable: true });
    }

    return {
        settle(id, json) {
            const entry = pending.get(id);
            if (!entry) throw new ErrorCtor("Unknown tool response");
            pending.delete(id);
            const response = parse(json);
            if ("error" in response) entry.reject(new ErrorCtor(
                typeof response.error === "string" ? response.error : stringify(response.error)));
            else entry.resolve(response.output);
            pump();
        },
        run(code) {
            // At this boundary, compilation, execution, and output-serialization
            // errors become failed completions, never substitute success values.
            try {
                const promise = new AsyncFunction(code + "\n//# sourceURL=codemode.js")();
                promiseThen.call(promise, value => {
                    try {
                        if (value !== undefined) text(value);
                        done(true, "");
                    } catch (error) {
                        done(false, errorText(error));
                    }
                }, error => done(false, errorText(error)));
            } catch (error) {
                done(false, errorText(error));
            }
        },
        stalled() {
            if (!finished && pending.size === 0) {
                done(false, "The script is waiting on a promise that can never settle: no tool call is pending, and timers do not exist here.");
            }
        }
    };
})
