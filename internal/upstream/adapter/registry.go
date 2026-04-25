package adapter

import "fmt"

// New 按 type 构造对应适配器。
func New(typ string, p Params) (Adapter, error) {
	switch typ {
	case "openai":
		return NewOpenAI(p), nil
	case "gemini":
		return NewGemini(p), nil
	default:
		return nil, fmt.Errorf("adapter: unsupported type %q", typ)
	}
}
