package chatgpt

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

type turnstileFunc func(args ...any)

type turnstileSolveDebug struct {
	Stage            string
	Encoding         string
	DXLen            int
	PLen             int
	DecodedLen       int
	InstructionCount int
	StageCount       int
	ExecutedCount    int
	RecoveredCount   int
	ResultStage      int
	ResultInputLen   int
}

type turnstileOrderedMap struct {
	keys   []string
	values map[string]any
}

func newTurnstileOrderedMap() *turnstileOrderedMap {
	return &turnstileOrderedMap{values: make(map[string]any)}
}

func (m *turnstileOrderedMap) add(key string, value any) {
	if _, ok := m.values[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.values[key] = value
}

// SolveTurnstileToken executes the local Turnstile instruction stream returned
// by /backend-api/sentinel/chat-requirements/prepare. The algorithm is a direct
// Go port of chatgpt2api/utils/turnstile.py: base64 decode dx, XOR it with the
// request-local p token, JSON-decode the instruction list, then run the small
// opcode interpreter to produce the final base64 token for finalize.
func SolveTurnstileToken(dx, p string) string {
	result, _ := solveTurnstileToken(dx, p)
	return result
}

func solveTurnstileToken(dx, p string) (string, turnstileSolveDebug) {
	debug := turnstileSolveDebug{
		Stage: "start",
		DXLen: len(dx),
		PLen:  len(p),
	}
	decoded, encoding, err := decodeTurnstileDX(dx)
	if err != nil {
		debug.Stage = "decode_dx"
		return "", debug
	}
	debug.Encoding = encoding
	debug.DecodedLen = len(decoded)
	var tokenList []any
	dec := json.NewDecoder(strings.NewReader(xorTurnstileText(string(decoded), p)))
	dec.UseNumber()
	if err := dec.Decode(&tokenList); err != nil {
		debug.Stage = "decode_json"
		return "", debug
	}
	tokenList = turnstileNormalizeJSONValue(tokenList).([]any)
	debug.Stage = "execute"
	debug.InstructionCount = len(tokenList)

	process := make(map[string]any)
	start := time.Now()
	result := ""
	currentStage := 0

	process["1"] = turnstileFunc(func(args ...any) {
		if len(args) < 2 {
			return
		}
		e, okE := turnstileProcessKey(args[0])
		t, okT := turnstileProcessKey(args[1])
		if !okE || !okT {
			return
		}
		current, ok := process[e]
		if !ok {
			return
		}
		incoming, ok := process[t]
		if !ok {
			return
		}
		process[e] = xorTurnstileText(turnstileToStr(current), turnstileToStr(incoming))
	})
	process["2"] = turnstileFunc(func(args ...any) {
		if len(args) < 2 {
			return
		}
		e, ok := turnstileProcessKey(args[0])
		if !ok {
			return
		}
		process[e] = args[1]
	})
	process["3"] = turnstileFunc(func(args ...any) {
		if len(args) < 1 {
			return
		}
		s, ok := args[0].(string)
		if !ok {
			return
		}
		debug.ResultStage = currentStage
		debug.ResultInputLen = len(s)
		result = base64.StdEncoding.EncodeToString([]byte(s))
	})
	process["5"] = turnstileFunc(func(args ...any) {
		if len(args) < 2 {
			return
		}
		e, okE := turnstileProcessKey(args[0])
		t, okT := turnstileProcessKey(args[1])
		if !okE || !okT {
			return
		}
		current, ok := process[e]
		if !ok {
			return
		}
		incoming, ok := process[t]
		if !ok {
			return
		}
		if list, ok := turnstileAppendList(current); ok {
			process[e] = append(list, incoming)
			return
		}
		if turnstileIsPythonConcatValue(current) || turnstileIsPythonConcatValue(incoming) {
			process[e] = turnstileToStr(current) + turnstileToStr(incoming)
			return
		}
		process[e] = "NaN"
	})
	process["6"] = turnstileFunc(func(args ...any) {
		if len(args) < 3 {
			return
		}
		e, okE := turnstileProcessKey(args[0])
		t, okT := turnstileProcessKey(args[1])
		n, okN := turnstileProcessKey(args[2])
		if !okE || !okT || !okN {
			return
		}
		rawTV, ok := process[t]
		if !ok {
			return
		}
		rawNV, ok := process[n]
		if !ok {
			return
		}
		tv, okTV := rawTV.(string)
		nv, okNV := rawNV.(string)
		if !okTV || !okNV {
			return
		}
		value := tv + "." + nv
		if value == "window.document.location" {
			process[e] = "https://chatgpt.com/"
			return
		}
		process[e] = value
	})
	process["7"] = turnstileFunc(func(args ...any) {
		if len(args) < 1 {
			return
		}
		e, ok := turnstileProcessKey(args[0])
		if !ok {
			return
		}
		target, ok := process[e]
		if !ok {
			return
		}
		values := make([]any, 0, len(args)-1)
		for _, arg := range args[1:] {
			key, ok := turnstileProcessKey(arg)
			if !ok {
				return
			}
			value, ok := process[key]
			if !ok {
				return
			}
			values = append(values, value)
		}
		if target == "window.Reflect.set" {
			if len(values) < 3 {
				return
			}
			obj, ok := values[0].(*turnstileOrderedMap)
			if !ok {
				return
			}
			obj.add(turnstileKeyToStr(values[1]), values[2])
			return
		}
		if fn, ok := target.(turnstileFunc); ok {
			fn(values...)
		}
	})
	process["8"] = turnstileFunc(func(args ...any) {
		if len(args) < 2 {
			return
		}
		e, okE := turnstileProcessKey(args[0])
		t, okT := turnstileProcessKey(args[1])
		if !okE || !okT {
			return
		}
		value, ok := process[t]
		if !ok {
			return
		}
		process[e] = value
	})
	process["14"] = turnstileFunc(func(args ...any) {
		if len(args) < 2 {
			return
		}
		e, okE := turnstileProcessKey(args[0])
		t, okT := turnstileProcessKey(args[1])
		if !okE || !okT {
			return
		}
		value, ok := process[t]
		if !ok {
			return
		}
		s, ok := value.(string)
		if !ok {
			return
		}
		if parsed, ok := turnstileParseJSON(s); ok {
			process[e] = parsed
		}
	})
	process["15"] = turnstileFunc(func(args ...any) {
		if len(args) < 2 {
			return
		}
		e, okE := turnstileProcessKey(args[0])
		t, okT := turnstileProcessKey(args[1])
		if !okE || !okT {
			return
		}
		value, ok := process[t]
		if !ok {
			return
		}
		process[e] = turnstileJSONDumps(value)
	})
	process["17"] = turnstileFunc(func(args ...any) {
		if len(args) < 2 {
			return
		}
		e, okE := turnstileProcessKey(args[0])
		t, okT := turnstileProcessKey(args[1])
		if !okE || !okT {
			return
		}
		callArgs := make([]any, 0, len(args)-2)
		for _, arg := range args[2:] {
			key, ok := turnstileProcessKey(arg)
			if !ok {
				return
			}
			value, ok := process[key]
			if !ok {
				return
			}
			callArgs = append(callArgs, value)
		}
		target, ok := process[t]
		if !ok {
			return
		}
		switch target {
		case "window.performance.now":
			process[e] = (float64(time.Since(start).Nanoseconds()) + rand.Float64()) / 1e6 //nolint:gosec
		case "window.Object.create":
			process[e] = newTurnstileOrderedMap()
		case "window.Object.keys":
			if len(callArgs) > 0 && callArgs[0] == "window.localStorage" {
				process[e] = []string{
					"STATSIG_LOCAL_STORAGE_INTERNAL_STORE_V4",
					"STATSIG_LOCAL_STORAGE_STABLE_ID",
					"client-correlated-secret",
					"oai/apps/capExpiresAt",
					"oai-did",
					"STATSIG_LOCAL_STORAGE_LOGGING_REQUEST",
					"UiState.isNavigationCollapsed.1",
				}
			}
		case "window.Math.random":
			process[e] = rand.Float64() //nolint:gosec
		default:
			if fn, ok := target.(turnstileFunc); ok {
				fn(callArgs...)
			}
		}
	})
	process["18"] = turnstileFunc(func(args ...any) {
		if len(args) < 1 {
			return
		}
		e, ok := turnstileProcessKey(args[0])
		if !ok {
			return
		}
		value, ok := process[e]
		if !ok {
			return
		}
		decoded, err := base64.StdEncoding.DecodeString(turnstileToStr(value))
		if err != nil {
			return
		}
		process[e] = string(decoded)
	})
	process["19"] = turnstileFunc(func(args ...any) {
		if len(args) < 1 {
			return
		}
		e, ok := turnstileProcessKey(args[0])
		if !ok {
			return
		}
		value, ok := process[e]
		if !ok {
			return
		}
		process[e] = base64.StdEncoding.EncodeToString([]byte(turnstileToStr(value)))
	})
	process["20"] = turnstileFunc(func(args ...any) {
		if len(args) < 3 {
			return
		}
		e, okE := turnstileProcessKey(args[0])
		t, okT := turnstileProcessKey(args[1])
		n, okN := turnstileProcessKey(args[2])
		if !okE || !okT || !okN {
			return
		}
		left, ok := process[e]
		if !ok {
			return
		}
		right, ok := process[t]
		if !ok {
			return
		}
		if !turnstileEqual(left, right) {
			return
		}
		targetValue, ok := process[n]
		if !ok {
			return
		}
		target, ok := targetValue.(turnstileFunc)
		if !ok {
			return
		}
		values := make([]any, 0, len(args)-3)
		for _, arg := range args[3:] {
			key, ok := turnstileProcessKey(arg)
			if !ok {
				return
			}
			value, ok := process[key]
			if !ok {
				return
			}
			values = append(values, value)
		}
		target(values...)
	})
	process["21"] = turnstileFunc(func(args ...any) {})
	process["23"] = turnstileFunc(func(args ...any) {
		if len(args) < 2 {
			return
		}
		e, okE := turnstileProcessKey(args[0])
		t, okT := turnstileProcessKey(args[1])
		if !okE || !okT {
			return
		}
		source, ok := process[e]
		if !ok || source == nil {
			return
		}
		targetValue, ok := process[t]
		if !ok {
			return
		}
		target, ok := targetValue.(turnstileFunc)
		if ok {
			target(args[2:]...)
		}
	})
	process["24"] = turnstileFunc(func(args ...any) {
		if len(args) < 3 {
			return
		}
		e, okE := turnstileProcessKey(args[0])
		t, okT := turnstileProcessKey(args[1])
		n, okN := turnstileProcessKey(args[2])
		if !okE || !okT || !okN {
			return
		}
		rawTV, ok := process[t]
		if !ok {
			return
		}
		rawNV, ok := process[n]
		if !ok {
			return
		}
		tv, okTV := rawTV.(string)
		nv, okNV := rawNV.(string)
		if okTV && okNV {
			process[e] = tv + "." + nv
		}
	})
	process["9"] = tokenList
	process["10"] = "window"
	process["16"] = p

	program := tokenList
	const maxTurnstileStages = 8
	for stage := 0; stage < maxTurnstileStages; stage++ {
		currentStage = stage + 1
		debug.StageCount++
		for _, token := range program {
			instruction, ok := token.([]any)
			if !ok || len(instruction) == 0 {
				continue
			}
			opcode, ok := turnstileProcessKey(instruction[0])
			if !ok {
				continue
			}
			fn, ok := process[opcode].(turnstileFunc)
			if !ok {
				continue
			}
			func() {
				defer func() {
					if recover() != nil {
						debug.RecoveredCount++
					}
				}()
				fn(instruction[1:]...)
				debug.ExecutedCount++
			}()
		}
		if result != "" {
			break
		}
		nextProgram, ok := process["9"].([]any)
		if !ok || turnstileSameProgram(nextProgram, program) {
			break
		}
		program = nextProgram
	}

	if result == "" {
		debug.Stage = "no_result"
		return "", debug
	}
	debug.Stage = "ok"
	return result, debug
}

func turnstileSameProgram(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	return &a[0] == &b[0]
}

func decodeTurnstileDX(dx string) ([]byte, string, error) {
	type candidate struct {
		name string
		enc  *base64.Encoding
		text string
	}
	candidates := []candidate{
		{name: "std", enc: base64.StdEncoding, text: dx},
		{name: "raw_std", enc: base64.RawStdEncoding, text: dx},
		{name: "url", enc: base64.URLEncoding, text: dx},
		{name: "raw_url", enc: base64.RawURLEncoding, text: dx},
	}
	if rem := len(dx) % 4; rem != 0 {
		padded := dx + strings.Repeat("=", 4-rem)
		candidates = append(candidates,
			candidate{name: "std_padded", enc: base64.StdEncoding, text: padded},
			candidate{name: "url_padded", enc: base64.URLEncoding, text: padded},
		)
	}
	var lastErr error
	for _, c := range candidates {
		decoded, err := c.enc.DecodeString(c.text)
		if err == nil {
			return decoded, c.name, nil
		}
		lastErr = err
	}
	return nil, "", lastErr
}

func xorTurnstileText(text, key string) string {
	if key == "" {
		return text
	}
	textRunes := []rune(text)
	keyRunes := []rune(key)
	var out strings.Builder
	for i, ch := range textRunes {
		out.WriteRune(ch ^ keyRunes[i%len(keyRunes)])
	}
	return out.String()
}

func turnstileProcessKey(v any) (string, bool) {
	switch t := v.(type) {
	case int:
		return strconv.Itoa(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case float64:
		return turnstileFloatKey(t)
	case json.Number:
		if i, err := strconv.ParseInt(t.String(), 10, 64); err == nil {
			return strconv.FormatInt(i, 10), true
		}
		f, err := strconv.ParseFloat(t.String(), 64)
		if err != nil {
			return "", false
		}
		return turnstileFloatKey(f)
	default:
		return "", false
	}
}

func turnstileFloatKey(v float64) (string, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "", false
	}
	if math.Trunc(v) == v {
		return strconv.FormatInt(int64(v), 10), true
	}
	return strconv.FormatFloat(v, 'f', -1, 64), true
}

func turnstileToStr(value any) string {
	switch v := value.(type) {
	case nil:
		return "undefined"
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case string:
		switch v {
		case "window.Math":
			return "[object Math]"
		case "window.Reflect":
			return "[object Reflect]"
		case "window.performance":
			return "[object Performance]"
		case "window.localStorage":
			return "[object Storage]"
		case "window.Object":
			return "function Object() { [native code] }"
		case "window.Reflect.set":
			return "function set() { [native code] }"
		case "window.performance.now":
			return "function () { [native code] }"
		case "window.Object.create":
			return "function create() { [native code] }"
		case "window.Object.keys":
			return "function keys() { [native code] }"
		case "window.Math.random":
			return "function random() { [native code] }"
		default:
			return v
		}
	case float64:
		if math.IsNaN(v) {
			return "nan"
		}
		return turnstilePythonFloatString(v)
	case json.Number:
		return v.String()
	case []string:
		return strings.Join(v, ",")
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return fmt.Sprint(value)
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprint(value)
	}
}

func turnstileKeyToStr(value any) string {
	if value == nil {
		return "None"
	}
	return fmt.Sprint(value)
}

func turnstileAppendList(value any) ([]any, bool) {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		copy(out, v)
		return out, true
	case []string:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, item)
		}
		return out, true
	default:
		return nil, false
	}
}

func turnstileIsPythonConcatValue(value any) bool {
	switch value.(type) {
	case string, float64:
		return true
	default:
		return false
	}
}

func turnstileParseJSON(raw string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, false
	}
	return turnstileNormalizeJSONValue(out), true
}

func turnstileJSONDumps(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		return turnstileJSONString(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case json.Number:
		return v.String()
	case float64:
		if math.IsNaN(v) {
			return "NaN"
		}
		if math.IsInf(v, 1) {
			return "Infinity"
		}
		if math.IsInf(v, -1) {
			return "-Infinity"
		}
		return turnstilePythonFloatString(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, turnstileJSONDumps(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []string:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, turnstileJSONString(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		parts := make([]string, 0, len(v))
		for key, item := range v {
			parts = append(parts, turnstileJSONString(key)+": "+turnstileJSONDumps(item))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case *turnstileOrderedMap:
		parts := make([]string, 0, len(v.keys))
		for _, key := range v.keys {
			parts = append(parts, turnstileJSONString(key)+": "+turnstileJSONDumps(v.values[key]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return turnstileJSONString(fmt.Sprint(v))
		}
		return string(b)
	}
}

func turnstileNormalizeJSONValue(value any) any {
	switch v := value.(type) {
	case json.Number:
		raw := v.String()
		if strings.ContainsAny(raw, ".eE") {
			if f, err := strconv.ParseFloat(raw, 64); err == nil {
				return f
			}
			return raw
		}
		if i, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			return f
		}
		return raw
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = turnstileNormalizeJSONValue(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = turnstileNormalizeJSONValue(item)
		}
		return out
	default:
		return value
	}
}

func turnstilePythonFloatString(value float64) string {
	text := strconv.FormatFloat(value, 'f', -1, 64)
	if math.Trunc(value) == value && !strings.ContainsAny(text, ".eE") {
		return text + ".0"
	}
	return text
}

func turnstileJSONString(value string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return `""`
	}
	return strings.TrimSpace(b.String())
}

func turnstileEqual(a, b any) bool {
	switch av := a.(type) {
	case json.Number:
		if bv, ok := b.(json.Number); ok {
			return av.String() == bv.String()
		}
	case float64:
		if bv, ok := b.(float64); ok {
			return av == bv
		}
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}
