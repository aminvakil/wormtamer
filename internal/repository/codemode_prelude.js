// Adapted from @earendil-works/pi-codemode 1.0.3 and 1.5.0, runtime/prelude-source.ts.
// Copyright (c) 2025 Mario Zechner. MIT license: see pi-codemode.LICENSE.
(function (bridge) {
    "use strict";

    // The prelude and script share built-ins. Lock them before running model code
    // so prototype or global mutations cannot corrupt tool responses or output.
    // This protects bridge integrity, not the unrestricted Bash capability.
    //
    // Freeze every object reachable from the built-in globals, plus intrinsics
    // only reachable from instances (iterator and generator prototypes, %TypedArray%).
    // Freezing alone causes the "override mistake": inherited read-only data
    // properties reject instance assignments, including this.name in Error subclasses.
    // Common overrides use setters that define an own property on the instance.
    (function lockdown() {
        const OVERRIDABLE = new Set(["constructor", "name", "message", "toString", "toLocaleString", "valueOf", "toJSON"]);
        const seen = new Set([globalThis]);
        const queue = [];
        const add = value => {
            if ((typeof value === "object" && value !== null) || typeof value === "function") {
                if (!seen.has(value)) {
                    seen.add(value);
                    queue.push(value);
                }
            }
        };

        // Object-literal accessors have no own prototype; function expressions
        // would introduce prototype.constructor properties to convert without end.
        function allowOverride(object, key, value, enumerable) {
            const { get, set } = Object.getOwnPropertyDescriptor({
                get accessor() {
                    return value;
                },
                set accessor(next) {
                    if (this === object) {
                        throw new TypeError("Cannot assign to read only property '" + String(key) + "' of a built-in");
                    }
                    if ((typeof this !== "object" || this === null) && typeof this !== "function") return;
                    Object.defineProperty(this, key, { value: next, writable: true, enumerable: true, configurable: true });
                },
            }, "accessor");
            Object.defineProperty(object, key, { get, set, enumerable, configurable: false });
            add(get);
            add(set);
        }

        for (const key of Reflect.ownKeys(globalThis)) {
            const descriptor = Object.getOwnPropertyDescriptor(globalThis, key);
            add(descriptor.value);
            add(descriptor.get);
            add(descriptor.set);
            if ("value" in descriptor && descriptor.configurable) {
                Object.defineProperty(globalThis, key, { writable: false, configurable: false });
            }
        }
        add(Object.getPrototypeOf(function* () {}));
        add(Object.getPrototypeOf(async function () {}));
        add(Object.getPrototypeOf(async function* () {}));
        add(Object.getPrototypeOf(Int8Array));
        add(Object.getPrototypeOf([][Symbol.iterator]()));
        add(Object.getPrototypeOf(new Map()[Symbol.iterator]()));
        add(Object.getPrototypeOf(new Set()[Symbol.iterator]()));
        add(Object.getPrototypeOf(""[Symbol.iterator]()));
        add(Object.getPrototypeOf(/a/[Symbol.matchAll]("")));
        if (typeof Iterator === "function") {
            if (typeof Iterator.prototype.map === "function") add(Object.getPrototypeOf([].values().map(value => value)));
            if (typeof Iterator.from === "function") add(Object.getPrototypeOf(Iterator.from({ next() {} })));
        }

        while (queue.length > 0) {
            const object = queue.pop();
            add(Object.getPrototypeOf(object));
            const descriptors = Object.getOwnPropertyDescriptors(object);
            for (const key of Reflect.ownKeys(descriptors)) {
                const descriptor = descriptors[key];
                if ("value" in descriptor) {
                    add(descriptor.value);
                    if (descriptor.writable && descriptor.configurable &&
                        (object === Object.prototype || OVERRIDABLE.has(key))) {
                        allowOverride(object, key, descriptor.value, descriptor.enumerable);
                    }
                } else {
                    add(descriptor.get);
                    add(descriptor.set);
                }
            }
            Object.freeze(object);
        }
    })();
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

    function stringifyOutput(value) {
        return stringify(value, (_, nested) => nested instanceof ErrorCtor
            ? { ...nested, name: nested.name, message: nested.message }
            : nested);
    }

    function text(value) {
        if (finished) return;
        const rendered = value !== null && (typeof value === "object" || typeof value === "function")
            ? stringifyOutput(value) : String(value);
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
            const json = stringifyOutput(value);
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
