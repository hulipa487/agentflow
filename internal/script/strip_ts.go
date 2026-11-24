package script

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"
)

type tokKind int

const (
	tokIdent tokKind = iota
	tokNum
	tokStr
	tokRegex
	tokTplFull // `...` with no interpolation
	tokTplHead // `...${
	tokTplMid  // }...${
	tokTplTail // }...`
	tokPunct   // operators/delimiters
	tokArrow   // =>
	tokLT      // <
	tokGT      // >
	tokEOF
)

type token struct {
	kind    tokKind
	start   int
	end     int
	line    int
	raw     string
}

func (t token) isValueEnd() bool {
	switch t.kind {
	case tokNum, tokStr, tokRegex, tokTplFull, tokTplTail:
		return true
	case tokIdent:
		switch t.raw {
		case "return", "throw", "case", "delete", "void", "typeof", "in", "instanceof", "new", "await", "yield", "of", "else", "do":
			return false
		}
		return true
	case tokPunct:
		return t.raw == ")" || t.raw == "]" || t.raw == "++" || t.raw == "--"
	default:
		return false
	}
}

func isIdentStart(r rune) bool {
	return r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= 0x80 && unicode.IsLetter(r))
}

func isIdentCont(r rune) bool {
	return isIdentStart(r) || (r >= '0' && r <= '9') || (r >= 0x80 && unicode.IsDigit(r))
}

func canStartRegex(prev token) bool {
	if prev.kind == tokEOF {
		return true
	}
	if prev.kind == tokTplHead || prev.kind == tokTplMid {
		return true
	}
	if prev.kind == tokLT || prev.kind == tokGT || prev.kind == tokArrow {
		return true
	}
	if prev.kind == tokPunct {
		switch prev.raw {
		case ")", "]", "++", "--":
			return false
		default:
			return true
		}
	}
	if prev.kind == tokIdent {
		return !prev.isValueEnd()
	}
	return false
}

// lex strips nothing on its own; it yields non-trivia tokens referencing exact
// byte offsets into src.
func lex(src string) []token {
	var toks []token
	var tplStack []int
	braceDepth := 0
	line := 1
	n := len(src)
	i := 0
	prev := token{kind: tokEOF}

	emit := func(t token) {
		toks = append(toks, t)
		prev = t
	}

	scanTemplateCont := func(start int, kindHead, kindTail tokKind, startLine int) {
		for i < n {
			c := src[i]
			if c == '\n' {
				line++
				i++
				continue
			}
			if c == '\\' {
				i++
				if i < n {
					if src[i] == '\n' {
						line++
					}
					i++
				}
				continue
			}
			if c == '`' {
				i++
				emit(token{kind: kindTail, start: start, end: i, line: startLine, raw: src[start:i]})
				return
			}
			if c == '$' && i+1 < n && src[i+1] == '{' {
				i += 2
				tplStack = append(tplStack, braceDepth)
				emit(token{kind: kindHead, start: start, end: i, line: startLine, raw: src[start:i]})
				return
			}
			i++
		}
		emit(token{kind: kindTail, start: start, end: i, line: startLine, raw: src[start:i]})
	}

	for i < n {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\v' || c == '\f' {
			i++
			continue
		}
		if c == '\n' {
			line++
			i++
			continue
		}
		// Comments.
		if c == '/' && i+1 < n && src[i+1] == '/' {
			i += 2
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < n && src[i+1] == '*' {
			i += 2
			for i < n {
				if src[i] == '\n' {
					line++
				}
				if src[i] == '*' && i+1 < n && src[i+1] == '/' {
					i += 2
					break
				}
				i++
			}
			continue
		}
		// Strings.
		if c == '\'' || c == '"' {
			quote := c
			start := i
			startLine := line
			i++
			for i < n {
				if src[i] == '\n' {
					line++
				}
				if src[i] == '\\' {
					i++
					if i < n {
						if src[i] == '\n' {
							line++
						}
						i++
					}
					continue
				}
				if src[i] == quote {
					i++
					break
				}
				i++
			}
			emit(token{kind: tokStr, start: start, end: i, line: startLine, raw: src[start:i]})
			continue
		}
		// Template literals.
		if c == '`' {
			start := i
			startLine := line
			i++
			scanTemplateCont(start, tokTplHead, tokTplFull, startLine)
			continue
		}
		// Regex literals vs division.
		if c == '/' && canStartRegex(prev) {
			start := i
			startLine := line
			i++
			inClass := false
			for i < n {
				if src[i] == '\n' {
					break
				}
				if src[i] == '\\' {
					i++
					if i < n && src[i] != '\n' {
						i++
					}
					continue
				}
				if src[i] == '[' {
					inClass = true
				} else if src[i] == ']' {
					inClass = false
				} else if src[i] == '/' && !inClass {
					i++
					for i < n && ((src[i] >= 'a' && src[i] <= 'z') || (src[i] >= 'A' && src[i] <= 'Z')) {
						i++
					}
					break
				}
				i++
			}
			emit(token{kind: tokRegex, start: start, end: i, line: startLine, raw: src[start:i]})
			continue
		}
		// Numbers.
		if (c >= '0' && c <= '9') || (c == '.' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9') {
			start := i
			startLine := line
			i++
			for i < n {
				ch := src[i]
				if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_' || ch == '.' {
					if (ch == 'e' || ch == 'E') && i+1 < n && (src[i+1] == '+' || src[i+1] == '-') {
						i += 2
						continue
					}
					i++
					continue
				}
				break
			}
			emit(token{kind: tokNum, start: start, end: i, line: startLine, raw: src[start:i]})
			continue
		}
		// Identifiers / keywords.
		if r, w := utf8.DecodeRuneInString(src[i:]); isIdentStart(r) {
			start := i
			startLine := line
			i += w
			for i < n {
				r2, w2 := utf8.DecodeRuneInString(src[i:])
				if !isIdentCont(r2) {
					break
				}
				i += w2
			}
			emit(token{kind: tokIdent, start: start, end: i, line: startLine, raw: src[start:i]})
			continue
		}
		// Braces with template continuation check.
		if c == '{' {
			braceDepth++
			emit(token{kind: tokPunct, start: i, end: i + 1, line: line, raw: "{"})
			i++
			continue
		}
		if c == '}' {
			if len(tplStack) > 0 && tplStack[len(tplStack)-1] == braceDepth {
				tplStack = tplStack[:len(tplStack)-1]
				start := i
				startLine := line
				i++
				scanTemplateCont(start, tokTplMid, tokTplTail, startLine)
				continue
			}
			if braceDepth > 0 {
				braceDepth--
			}
			emit(token{kind: tokPunct, start: i, end: i + 1, line: line, raw: "}"})
			i++
			continue
		}
		// Multi-char operators. Keep single '<' and '>' unmerged when not paired
		// with '=' or '<<=' / '>>=' so nested generics like Array<Promise<T>> split cleanly.
		start := i
		startLine := line
		rem := src[i:]
		matched := ""
		for _, op := range []string{
			"===", "!==", ">>>=", ">>>", "<<=", ">>=", "...", "**=", "??=", "&&=", "||=",
			"=>", "==", "!=", "<=", ">=", "&&", "||", "??", "?.", "++", "--", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<", "**",
		} {
			if strings.HasPrefix(rem, op) {
				matched = op
				break
			}
		}
		if matched != "" {
			i += len(matched)
			kind := tokPunct
			if matched == "=>" {
				kind = tokArrow
			}
			emit(token{kind: kind, start: start, end: i, line: startLine, raw: matched})
			continue
		}
		i++
		kind := tokPunct
		if c == '<' {
			kind = tokLT
		} else if c == '>' {
			kind = tokGT
		}
		emit(token{kind: kind, start: start, end: i, line: startLine, raw: src[start:i]})
	}
	emit(token{kind: tokEOF, start: n, end: n, line: line})
	return toks
}

// StripTS removes erasable TypeScript syntax (interfaces, type aliases,
// type-only imports, parameter/variable/return annotations, generics, `as`,
// `satisfies`, `declare`, and `export` modifiers) while replacing removed bytes
// with spaces and preserving all newlines so runtime stack traces retain exact
// 1-based line and column positions.
func StripTS(src string) string {
	out := []byte(src)
	toks := lex(src)
	n := len(toks) - 1 // exclude tokEOF

	blanked := make([]bool, n)
	blankRange := func(start, end int) {
		if start < 0 {
			start = 0
		}
		if end > len(out) {
			end = len(out)
		}
		for k := start; k < end; k++ {
			if out[k] != '\n' && out[k] != '\r' {
				out[k] = ' '
			}
		}
		for idx := 0; idx < n; idx++ {
			if toks[idx].start >= start && toks[idx].end <= end {
				blanked[idx] = true
			}
		}
	}
	blankTok := func(idx int) {
		if idx >= 0 && idx < n {
			blanked[idx] = true
			blankRange(toks[idx].start, toks[idx].end)
		}
	}
	writeAtTok := func(idx int, replacement string) {
		t := toks[idx]
		blankTok(idx)
		for k := 0; k < len(replacement) && t.start+k < t.end; k++ {
			out[t.start+k] = replacement[k]
		}
	}

	var exportedNames []string
	var exportAliases [][2]string // [exportedName, localName]
	hasDefaultExport := false

	// Bracket helpers over toks.
	findMatchingBrace := func(openIdx int) int {
		depth := 0
		for j := openIdx; j < n; j++ {
			if toks[j].raw == "{" {
				depth++
			} else if toks[j].raw == "}" {
				depth--
				if depth == 0 {
					return j
				}
			}
		}
		return n - 1
	}

	findMatchingParen := func(openIdx int) int {
		depth := 0
		for j := openIdx; j < n; j++ {
			if toks[j].raw == "(" {
				depth++
			} else if toks[j].raw == ")" {
				depth--
				if depth == 0 {
					return j
				}
			}
		}
		return n - 1
	}

	// findMatchingAngle scans a balanced <...> list, ignoring `=>` arrows inside.
	// Returns the index of the closing '>' or -1 if malformed / looks like comparison math.
	findMatchingAngle := func(openIdx int) int {
		depth := 0
		paren, brace, bracket := 0, 0, 0
		for j := openIdx; j < n; j++ {
			t := toks[j]
			if t.kind == tokArrow {
				continue
			}
			switch t.raw {
			case "(":
				paren++
			case ")":
				paren--
			case "{":
				brace++
			case "}":
				brace--
			case "[":
				bracket++
			case "]":
				bracket--
			case ";":
				if brace == 0 {
					return -1
				}
			case "&&", "||":
				if paren == 0 && brace == 0 && bracket == 0 {
					return -1
				}
			}
			if paren < 0 || brace < 0 || bracket < 0 {
				return -1
			}
			if t.kind == tokLT {
				depth++
			} else if t.kind == tokGT {
				depth--
				if depth == 0 {
					return j
				}
			}
		}
		return -1
	}

	// scanTypeEnd returns the token index of the FIRST token AFTER the type
	// expression starting at idx.
	scanTypeEnd := func(idx int, stopOnColon, stopOnQuestion, stopOnLTForExpr, allowNewlineASI bool) int {
		paren, brace, bracket, angle := 0, 0, 0, 0
		j := idx
		// Track whether we've consumed a primary type atom so we know when `{`
		// starts a function body rather than a `{ ... }` object type, or when newline triggers ASI.
		wantType := true
		for j < n {
			t := toks[j]
			atTop := paren == 0 && brace == 0 && bracket == 0 && angle == 0

			if t.kind == tokArrow {
				wantType = true
				j++
				continue
			}

			if atTop {
				if !wantType {
					// Next token might terminate the type expression.
					if allowNewlineASI && j > idx && t.line > toks[j-1].line {
						continues := t.raw == "|" || t.raw == "&" || t.raw == "." || t.raw == "[" || t.raw == "extends" || t.raw == "is" || t.kind == tokLT || t.kind == tokArrow
						if !continues {
							return j
						}
					}
					switch t.raw {
					case ";", ",", ")", "]", "}", "=":
						return j
					case ":":
						if stopOnColon {
							return j
						}
					case "?":
						if stopOnQuestion {
							return j
						}
					case "{":
						// Function/method body `{ ... }` after return type!
						return j
					case "++", "--", "+", "-", "*", "/", "%", "&&", "||", "??", "==", "===", "!=", "!==", "<=", ">=", "as", "satisfies", "in", "instanceof", "=>":
						if t.raw != "=>" {
							return j
						}
					case ">":
						return j
					}
					// If we already parsed a type atom (`wantType == false`) and see another identifier
					// that isn't `is` or `extends` or `as`? Wait: newline already checked; same-line stmt?
					if t.kind == tokIdent && t.raw != "is" && t.raw != "extends" && !strings.HasPrefix(src[t.start:], "[") {
						// e.g. `const x: string y = 1` or `a as Foo b`
						return j
					}
				} else if t.raw == ";" || t.raw == "," || t.raw == ")" || t.raw == "]" || t.raw == "}" || t.raw == "=" {
					return j
				}
			}

			switch t.raw {
			case "(":
				paren++
				wantType = true
				j++
				continue
			case ")":
				if paren > 0 {
					paren--
				}
				wantType = false
				j++
				continue
			case "{":
				brace++
				wantType = true
				j++
				continue
			case "}":
				if brace > 0 {
					brace--
				}
				wantType = false
				j++
				continue
			case "[":
				bracket++
				wantType = true
				j++
				continue
			case "]":
				if bracket > 0 {
					bracket--
				}
				wantType = false
				j++
				continue
			}

			if t.kind == tokLT {
				if atTop && stopOnLTForExpr && wantType {
					return j
				}
				angle++
				wantType = true
				j++
				continue
			}
			if t.kind == tokGT {
				if angle > 0 {
					angle--
				}
				wantType = false
				j++
				continue
			}

			if t.raw == "|" || t.raw == "&" || t.raw == "." || t.raw == "typeof" || t.raw == "keyof" || t.raw == "infer" || t.raw == "readonly" || t.raw == "extends" || t.raw == "is" || t.raw == "asserts" || t.raw == "..." {
				wantType = true
			} else if atTop && t.raw == ":" && !stopOnColon {
				wantType = true
			} else {
				wantType = false
			}
			j++
		}
		return n
	}

	// scanArrowOrMethodReturnEnd scans return type after `): ` stopping BEFORE `=>` or `{`.
	scanReturnTypeEnd := func(idx int) int {
		paren, brace, bracket, angle := 0, 0, 0, 0
		j := idx
		wantType := true
		hasPredicate := false
		for j < n {
			t := toks[j]
			atTop := paren == 0 && brace == 0 && bracket == 0 && angle == 0
			if atTop {
				if t.kind == tokArrow {
					// Return type stops right BEFORE `=>` unless `=>` IS inside a function return type `(): (x: number) => void => {}`!
					// Check: if wantType is true OR if we look ahead from here and see another `=>` / `{` inside arrow?
					// Rule: for `(): (a: number) => void { body }`, look if another `=>` or `{` follows on the return type, else `=>` ends return type!
					if !wantType {
						return j
					}
				}
				if !wantType && t.raw == "{" {
					return j
				}
				if t.raw == ";" || t.raw == "," || t.raw == ")" || t.raw == "]" || t.raw == "}" || t.raw == "=" {
					return j
				}
				if !wantType && j > idx && t.line > toks[j-1].line {
					continues := t.raw == "|" || t.raw == "&" || t.raw == "." || t.raw == "[" || t.raw == "is" || t.kind == tokLT || (t.kind == tokArrow && hasPredicate)
					if !continues {
						return j
					}
				}
			}
			switch t.raw {
			case "(":
				paren++
				wantType = true
			case ")":
				if paren > 0 {
					paren--
				}
				wantType = false
			case "{":
				brace++
				wantType = true
			case "}":
				if brace > 0 {
					brace--
				}
				wantType = false
			case "[":
				bracket++
				wantType = true
			case "]":
				if bracket > 0 {
					bracket--
				}
				wantType = false
			default:
				if t.kind == tokLT {
					angle++
					wantType = true
				} else if t.kind == tokGT {
					if angle > 0 {
						angle--
					}
					wantType = false
				} else if t.kind == tokArrow {
					// `=>` inside paren/angle/object or after `(` -> function type return type continues!
					wantType = true
				} else if t.raw == "|" || t.raw == "&" || t.raw == "." || t.raw == "typeof" || t.raw == "keyof" || t.raw == "readonly" || t.raw == "is" || t.raw == "asserts" {
					if t.raw == "is" {
						hasPredicate = true
					}
					wantType = true
				} else {
					wantType = false
				}
			}
			j++
		}
		return n
	}

	isStmtBoundary := func(idx int) bool {
		prevIdx := idx - 1
		for prevIdx >= 0 && blanked[prevIdx] {
			prevIdx--
		}
		if prevIdx < 0 {
			return true
		}
		p := toks[prevIdx]
		if p.raw == ";" || p.raw == "{" || p.raw == "}" {
			return true
		}
		if p.line < toks[idx].line {
			// Check previous token doesn't naturally precede an expression continuation like `.`, `(`, `[`
			switch p.raw {
			case ".", "?.", "(", "[", ",", "=", "=>", ":", "?", "+", "-", "*", "/", "%", "&&", "||", "??":
				return false
			}
			return true
		}
		return false
	}

	// Track ternary depth per enclosing bracket frame so `cond ? fn() : b` does
	// not misread `: b` after `)` as a return type annotation.
	type frame struct {
		isParamList bool
		paramState  int // 0=expecting binding, 1=after binding (expect ?/:), 2=after = (default expr)
		paramDepth  int
		isClassBody bool
		ternary     int
	}
	stack := []frame{{}}
	top := func() *frame { return &stack[len(stack)-1] }

	// Pre-identify function/method/arrow parameter parens and class bodies.
	var processRange func(startIdx, endIdx int)
	processRange = func(startIdx, endIdx int) {
		i := startIdx
		for i < endIdx {
			t := toks[i]

			// 1. Type-only imports: `import type ...`
			if t.raw == "import" && isStmtBoundary(i) && i+1 < endIdx && toks[i+1].raw == "type" {
				j := i + 2
				for j < endIdx && toks[j].raw != ";" {
					if j > i+2 && toks[j].line > toks[j-1].line && toks[j-1].kind == tokStr {
						break
					}
					j++
				}
				if j < endIdx && toks[j].raw == ";" {
					j++
				}
				blankRange(toks[i].start, toks[j-1].end)
				i = j
				continue
			}

			// 2. `export ...` handling
			if t.raw == "export" && isStmtBoundary(i) && i+1 < endIdx {
				nxt := toks[i+1]
				if nxt.raw == "interface" || nxt.raw == "type" || nxt.raw == "declare" {
					blankTok(i)
					i++
					continue
				}
				if nxt.raw == "default" {
					hasDefaultExport = true
					blankTok(i)
					writeAtTok(i+1, "__af=  ")
					i += 2
					continue
				}
				if nxt.raw == "{" {
					// `export { a, b as c };`
					closeIdx := findMatchingBrace(i + 1)
					for k := i + 2; k < closeIdx; k++ {
						if toks[k].kind != tokIdent {
							continue
						}
						localName := toks[k].raw
						expName := localName
						if k+2 < closeIdx && toks[k+1].raw == "as" && toks[k+2].kind == tokIdent {
							expName = toks[k+2].raw
							k += 2
						}
						exportAliases = append(exportAliases, [2]string{expName, localName})
					}
					end := closeIdx + 1
					if end < endIdx && toks[end].raw == ";" {
						end++
					}
					blankRange(toks[i].start, toks[end-1].end)
					i = end
					continue
				}
				// `export [async] function name` / `export const|let|var name` / `export class name`
				blankTok(i)
				j := i + 1
				if j < endIdx && toks[j].raw == "async" {
					j++
				}
				if j < endIdx && (toks[j].raw == "function" || toks[j].raw == "class") {
					k := j + 1
					if k < endIdx && toks[k].raw == "*" {
						k++
					}
					if k < endIdx && toks[k].kind == tokIdent {
						exportedNames = append(exportedNames, toks[k].raw)
					}
				} else if j < endIdx && (toks[j].raw == "const" || toks[j].raw == "let" || toks[j].raw == "var") {
					if j+1 < endIdx && toks[j+1].kind == tokIdent {
						exportedNames = append(exportedNames, toks[j+1].raw)
					}
				}
				i++
				continue
			}

			// 3. `interface Foo ... { ... }`
			if t.raw == "interface" && isStmtBoundary(i) && i+1 < endIdx && toks[i+1].kind == tokIdent {
				j := i + 2
				for j < endIdx && toks[j].raw != "{" {
					j++
				}
				if j < endIdx && toks[j].raw == "{" {
					closeIdx := findMatchingBrace(j)
					end := closeIdx + 1
					if end < endIdx && toks[end].raw == ";" {
						end++
					}
					blankRange(toks[i].start, toks[end-1].end)
					i = end
					continue
				}
			}

			// 4. `type Name = ...` or `type { ... }`
			if t.raw == "type" && isStmtBoundary(i) && i+1 < endIdx {
				nxt := toks[i+1]
				if nxt.raw == "{" {
					closeIdx := findMatchingBrace(i + 1)
					end := closeIdx + 1
					if end < endIdx && toks[end].raw == ";" {
						end++
					}
					blankRange(toks[i].start, toks[end-1].end)
					i = end
					continue
				}
				if nxt.kind == tokIdent {
					j := i + 2
					if j < endIdx && toks[j].kind == tokLT {
						if closeAngle := findMatchingAngle(j); closeAngle >= 0 {
							j = closeAngle + 1
						}
					}
					if j < endIdx && toks[j].raw == "=" {
						end := scanTypeEnd(j+1, false, false, false, true)
						if end < endIdx && toks[end].raw == ";" {
							end++
						}
						blankRange(toks[i].start, toks[end-1].end)
						i = end
						continue
					}
				}
			}

			// 5. `declare ...` ambient declarations
			if t.raw == "declare" && isStmtBoundary(i) && i+1 < endIdx {
				nxt := toks[i+1].raw
				if nxt == "const" || nxt == "let" || nxt == "var" || nxt == "function" || nxt == "async" || nxt == "class" || nxt == "interface" || nxt == "type" || nxt == "namespace" || nxt == "module" || nxt == "global" || nxt == "enum" {
					j := i + 1
					for j < endIdx {
						if toks[j].raw == "{" {
							closeIdx := findMatchingBrace(j)
							end := closeIdx + 1
							if end < endIdx && toks[end].raw == ";" {
								end++
							}
							blankRange(toks[i].start, toks[end-1].end)
							i = end
							break
						}
						if toks[j].raw == ";" {
							blankRange(toks[i].start, toks[j].end)
							i = j + 1
							break
						}
						if j > i+1 && toks[j].line > toks[j-1].line && toks[j-1].raw != "=" {
							blankRange(toks[i].start, toks[j-1].end)
							i = j
							break
						}
						j++
					}
					if j >= endIdx {
						blankRange(toks[i].start, toks[endIdx-1].end)
						i = endIdx
					}
					continue
				}
			}

			// 6. Track parameter state within an active parameter list
			f := top()
			if f.isParamList && f.paramDepth == 0 {
				if t.raw == "," {
					f.paramState = 0
					i++
					continue
				}
				if f.paramState == 0 {
					if t.raw == "..." || t.raw == "public" || t.raw == "private" || t.raw == "protected" || t.raw == "readonly" || t.raw == "override" {
						if t.raw != "..." {
							blankTok(i)
						}
						i++
						continue
					}
					if t.kind == tokIdent {
						f.paramState = 1
						i++
						continue
					}
					if t.raw == "{" || t.raw == "[" {
						// Destructuring pattern inside parameter: let bracket tracker handle it,
						// when it closes at paramDepth==0 transition to paramState=1.
						f.paramState = 1
						f.paramDepth++
						stack = append(stack, frame{})
						i++
						continue
					}
				} else if f.paramState == 1 {
					if t.raw == "?" {
						blankTok(i)
						i++
						continue
					}
					if t.raw == ":" {
						end := scanTypeEnd(i+1, true, false, false, false)
						if end > i {
							blankRange(toks[i].start, toks[end-1].end)
							i = end
							continue
						}
					}
					if t.raw == "=" {
						f.paramState = 2
						i++
						continue
					}
				}
			}

			// 7. Variable declarations: `const|let|var [name | {...} | [...]] [!]: Type`
			if (t.raw == "const" || t.raw == "let" || t.raw == "var") && i+1 < endIdx && (i == 0 || toks[i-1].raw != ".") {
				k := i + 1
				if toks[k].kind == tokIdent {
					k++
				} else if toks[k].raw == "{" {
					// Notice variable destructuring `{ a, b }: Type = val` — wait: recurse on `{}`? No TS inside binding except nested defaults, skip to matching `}`
					k = findMatchingBrace(k) + 1
				} else if toks[k].raw == "[" {
					depth := 1
					k++
					for k < endIdx && depth > 0 {
						if toks[k].raw == "[" {
							depth++
						} else if toks[k].raw == "]" {
							depth--
						}
						k++
					}
				} else {
					i++
					continue
				}
				if k < endIdx && toks[k].raw == "!" {
					blankTok(k)
					k++
				}
				if k < endIdx && toks[k].raw == ":" {
					end := scanTypeEnd(k+1, false, false, false, true)
					if end > k {
						blankRange(toks[k].start, toks[end-1].end)
						i = end
						continue
					}
				}
			}

			// 8. Class body modifiers and field types: `class Foo<T> implements Bar { private x!: string; }`
			if t.raw == "class" && i+1 < endIdx && (i == 0 || toks[i-1].raw != ".") {
				j := i + 1
				if j < endIdx && toks[j].kind == tokIdent {
					j++
				}
				if j < endIdx && toks[j].kind == tokLT {
					if closeA := findMatchingAngle(j); closeA >= 0 {
						blankRange(toks[j].start, toks[closeA].end)
						j = closeA + 1
					}
				}
				for j < endIdx && toks[j].raw != "{" {
					if toks[j].raw == "implements" {
						k := j + 1
						for k < endIdx && toks[k].raw != "{" {
							k++
						}
						blankRange(toks[j].start, toks[k-1].end)
						j = k
						break
					}
					if toks[j].raw == "extends" && j+1 < endIdx {
						// Strip generic args on superclass if any
						k := j + 1
						for k < endIdx && toks[k].raw != "{" && toks[k].raw != "implements" {
							if toks[k].kind == tokLT {
								if closeA := findMatchingAngle(k); closeA >= 0 {
									blankRange(toks[k].start, toks[closeA].end)
									k = closeA + 1
									continue
								}
							}
							k++
						}
						j = k
						continue
					}
					j++
				}
				if j < endIdx && toks[j].raw == "{" {
					stack = append(stack, frame{isClassBody: true})
					i = j + 1
					continue
				}
			}

			if f.isClassBody && isStmtBoundary(i) {
				if t.raw == "public" || t.raw == "private" || t.raw == "protected" || t.raw == "readonly" || t.raw == "override" || t.raw == "abstract" {
					blankTok(i)
					i++
					continue
				}
				if t.kind == tokIdent && i+1 < endIdx && (toks[i+1].raw == ":" || toks[i+1].raw == "?" || toks[i+1].raw == "!") {
					k := i + 1
					if toks[k].raw == "?" || toks[k].raw == "!" {
						blankTok(k)
						k++
					}
					if k < endIdx && toks[k].raw == ":" {
						end := scanTypeEnd(k+1, false, false, false, true)
						if end > k {
							blankRange(toks[k].start, toks[end-1].end)
							i = end
							continue
						}
					}
				}
			}

			// 9. Generic parameter / argument lists `<...>`
			if t.kind == tokLT {
				isFuncOrClassGen := i > 0 && toks[i-1].kind == tokIdent && i > 1 && (toks[i-2].raw == "function" || toks[i-2].raw == "class")
				isArrowGen := i == 0 || !toks[i-1].isValueEnd()
				isCallGen := i > 0 && (toks[i-1].kind == tokIdent || toks[i-1].raw == ")" || toks[i-1].raw == "]")

				if closeA := findMatchingAngle(i); closeA >= 0 {
					nxt := toks[closeA+1]
					stripped := false
					if isFuncOrClassGen && nxt.raw == "(" {
						stripped = true
					} else if isArrowGen && nxt.raw == "(" {
						closeP := findMatchingParen(closeA + 1)
						if closeP+1 < endIdx && (toks[closeP+1].kind == tokArrow || toks[closeP+1].raw == ":") {
							stripped = true
						}
					} else if isCallGen && (nxt.raw == "(" || nxt.kind == tokTplFull || nxt.kind == tokTplHead) {
						// Method/call generic: `fn<string>(x)` or `fn<T>` followed by template tag
						stripped = true
					}
					if stripped {
						blankRange(toks[i].start, toks[closeA].end)
						i = closeA + 1
						continue
					}
				}
			}

			// 10. Ternary tracking
			if t.raw == "?" {
				f.ternary++
				i++
				continue
			}
			if t.raw == ":" && f.ternary > 0 {
				f.ternary--
				i++
				continue
			}

			// 11. Postfix `as Type` / `satisfies Type`
			if (t.raw == "as" || t.raw == "satisfies") && i > 0 && i+1 < endIdx {
				prev := toks[i-1]
				if prev.isValueEnd() || (prev.raw == "}" && prev.line == t.line) {
					nxt := toks[i+1]
					if nxt.raw != ":" && nxt.raw != "=" && nxt.raw != "," && nxt.raw != ";" && nxt.raw != ")" && nxt.raw != "}" && nxt.raw != "]" {
						end := scanTypeEnd(i+1, true, true, true, true)
						if end > i+1 {
							blankRange(toks[i].start, toks[end-1].end)
							i = end
							continue
						}
					}
				}
			}

			// 12. Bracket stack push / pop + parameter list + return type detection
			switch t.raw {
			case "(":
				isParams := false
				// function [name] (...)
				if i >= 1 && toks[i-1].raw == "function" {
					isParams = true
				} else if i >= 2 && toks[i-2].raw == "function" && (toks[i-1].kind == tokIdent || toks[i-1].raw == "*") {
					isParams = true
				} else if i >= 3 && toks[i-3].raw == "function" && toks[i-2].raw == "*" && toks[i-1].kind == tokIdent {
					isParams = true
				} else if i >= 1 && toks[i-1].raw == "catch" {
					isParams = true
				} else {
					closeP := findMatchingParen(i)
					if closeP+1 < endIdx {
						after := toks[closeP+1]
						if after.kind == tokArrow {
							isParams = true
						} else if after.raw == ":" && top().ternary == 0 {
							retEnd := scanReturnTypeEnd(closeP + 2)
							if retEnd < endIdx && (toks[retEnd].kind == tokArrow || toks[retEnd].raw == "{") {
								isParams = true
							}
						} else if after.raw == "{" && i >= 1 && toks[i-1].kind == tokIdent {
							switch toks[i-1].raw {
							case "if", "for", "while", "with", "switch", "catch":
							default:
								isParams = true
							}
						}
					}
				}
				stack = append(stack, frame{isParamList: isParams})
				i++
				continue

			case "{", "[":
				if f.isParamList {
					f.paramDepth++
				}
				stack = append(stack, frame{})
				i++
				continue

			case ")", "}", "]":
				wasParamList := f.isParamList && f.paramDepth == 0 && t.raw == ")"
				if len(stack) > 1 {
					stack = stack[:len(stack)-1]
				}
				if len(stack) > 0 && top().isParamList && top().paramDepth > 0 {
					top().paramDepth--
				}
				// Check for return type right after closing `)`: `): ReturnType =>` or `): ReturnType {`
				if wasParamList && i+1 < endIdx && toks[i+1].raw == ":" && top().ternary == 0 {
					// Not `case (x):`
					end := scanReturnTypeEnd(i + 2)
					if end < endIdx && (toks[end].kind == tokArrow || toks[end].raw == "{") {
						blankRange(toks[i+1].start, toks[end-1].end)
						i = end
						continue
					}
				}
				i++
				continue
			}

			i++
		}
	}

	processRange(0, n)

	var buf bytes.Buffer
	buf.Write(out)
	if hasDefaultExport || len(exportedNames) > 0 || len(exportAliases) > 0 {
		buf.WriteString("\n;")
		if hasDefaultExport {
			buf.WriteString(`if(typeof __af!=="undefined")exports.default=__af;`)
		}
		for _, name := range exportedNames {
			buf.WriteString(`try{exports["` + name + `"]=` + name + `}catch(_){};`)
		}
		for _, pair := range exportAliases {
			buf.WriteString(`try{exports["` + pair[0] + `"]=` + pair[1] + `}catch(_){};`)
		}
	}
	return buf.String()
}
