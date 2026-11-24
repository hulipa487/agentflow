package script

import (
	"strings"
	"testing"

	"github.com/dop251/goja"
)

func evalStrippedTS(t *testing.T, ts string) (string, *goja.Runtime, error) {
	t.Helper()
	js := StripTS(ts)
	rt := goja.New()
	_, err := rt.RunString(js)
	return js, rt, err
}

func TestStripTSInterfacesTypesAndImports(t *testing.T) {
	ts := strings.Join([]string{
		`import type { Host, Request } from "./af";`,
		`import type Foo from "./foo";`,
		``,
		`export interface ChargeArgs {`,
		`  amount: number;`,
		`  currency?: string;`,
		`  meta: {`,
		`    orderId: string;`,
		`    tags: Array<string>;`,
		`  };`,
		`}`,
		``,
		`export type Currency = "usd" | "eur" | {`,
		`  custom: string;`,
		`};`,
		``,
		`declare function nativeHelper(x: number): void;`,
		``,
		`export function formatAmount(args: ChargeArgs): string {`,
		`  const cur: Currency = args.currency ?? "usd";`,
		"  return `${args.amount}:${cur}`;",
		`}`,
	}, "\n")

	js, rt, err := evalStrippedTS(t, ts)
	if err != nil {
		t.Fatalf("stripped JS failed to run: %v\n--- stripped JS ---\n%s", err, js)
	}
	_, err = rt.RunString(`
		var res = formatAmount({ amount: 42, meta: { orderId: "o1", tags: ["a"] } });
		if (res !== "42:usd") { throw new Error("unexpected result: " + res); }
	`)
	if err != nil {
		t.Fatalf("invocation failed: %v", err)
	}
}

func TestStripTSGenericsArrowsAsSatisfiesAndLinePreservation(t *testing.T) {
	ts := strings.Join([]string{
		`function identity<T>(x: T): T {`,
		`  return x;`,
		`}`,
		``,
		`const mapItems = <T,>(items: Array<T>, fn: (item: T, idx?: number) => string): Array<string> => {`,
		`  return items.map((it: T, i: number): string => fn(it, i));`,
		`};`,
		``,
		`class Greeter<T extends string> {`,
		`  private prefix: string = "hi";`,
		`  readonly suffix!: string;`,
		`  constructor(prefix: T) {`,
		`    this.prefix = prefix as string;`,
		`  }`,
		`  greet(name: string): string {`,
		"    return `${this.prefix} ${name}`;",
		`  }`,
		`}`,
		``,
		`const check = (a: number, b: number): boolean => a < b && b > 0;`,
		`const ternary = (ok: boolean): string => ok ? identity<string>("yes") : ("no" as string);`,
		`const obj = { a: 1, label: "x:y", pattern: /a\/b:c/g, div: 10 / 2 };`,
		`const sat = { host: "api.github.com" } satisfies Record<string, string>;`,
		``,
		`function boom(x: number): number {`,
		`  const val: number = x + 1;`,
		`  throw new Error("line-check");`,
		`}`,
	}, "\n")

	js, rt, err := evalStrippedTS(t, ts)
	if err != nil {
		t.Fatalf("stripped JS failed to run: %v\n--- stripped JS ---\n%s", err, js)
	}
	v, err := rt.RunString(`
		const g = new Greeter("hello");
		const out = mapItems(["world"], (s) => g.greet(s))[0];
		out + "|" + String(check(1, 2)) + "|" + ternary(true) + "|" + String(obj.div) + "|" + sat.host;
	`)
	if err != nil {
		t.Fatalf("runtime eval failed: %v", err)
	}
	if got := v.String(); got != "hello world|true|yes|5|api.github.com" {
		t.Fatalf("got %q", got)
	}

	// Verify runtime stack trace line numbers align 1-to-1 with line 27 in original .ts source.
	prog, err := Compile("sample.ts", ts, true)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	rt2 := NewRuntime(RuntimeOptions{})
	if _, err := rt2.RunProgram(prog); err != nil {
		t.Fatal(err)
	}
	_, err = rt2.RunString(`boom(5)`)
	if err == nil || !strings.Contains(err.Error(), "sample.ts:27:") {
		t.Fatalf("expected stack trace to report line 27 in sample.ts, got: %v", err)
	}
}

func TestStripTSExportForms(t *testing.T) {
	ts := `
export const run = (args: { n: number }): number => args.n * 2;
export function ping(): string { return "ok"; }
const hidden = 42;
export { hidden as answer };
export default { version: 1 };
`
	js := StripTS(ts)
	rt := NewRuntime(RuntimeOptions{})
	if _, err := rt.RunString(`var module = { exports: {} }; var exports = module.exports;`); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RunString(js); err != nil {
		t.Fatalf("eval failed: %v\n%s", err, js)
	}
	v, err := rt.RunString(`exports.run({ n: 7 }) + ":" + exports.ping() + ":" + exports.answer + ":" + exports.default.version`)
	if err != nil {
		t.Fatalf("export lookup failed: %v", err)
	}
	if got := v.String(); got != "14:ok:42:1" {
		t.Fatalf("got %q, want 14:ok:42:1", got)
	}
}
