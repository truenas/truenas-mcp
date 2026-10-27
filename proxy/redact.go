package proxy

import (
	"bytes"
	"encoding/json"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
)

// redactResponse returns resp with secrets removed from tool call results
// (text content and structuredContent) and error messages, the same redaction the server applies in
// handleToolsCall. Other results (initialize, tools/list) carry no TrueNAS
// data and are left untouched, so tool input schemas that mention
// "password" are not altered. resp is not modified.
func redactResponse(resp *mcp.Response) *mcp.Response {
	if resp == nil {
		return nil
	}
	out := *resp
	switch r := resp.Result.(type) {
	case mcp.ToolCallResult:
		out.Result = redactToolCallResult(r)
	case *mcp.ToolCallResult:
		if r != nil {
			red := redactToolCallResult(*r)
			out.Result = &red
		}
	case map[string]interface{}:
		if red, ok := redactToolCallMap(r); ok {
			out.Result = red
		}
	case json.RawMessage:
		if v, ok := decodeResult(r); ok {
			if red, ok := redactToolCallMap(v); ok {
				out.Result = red
			}
		}
	}
	if resp.Error != nil {
		e := *resp.Error
		e.Message = redact.Text(e.Message)
		if e.Data != nil {
			e.Data = redact.Value(e.Data)
		}
		out.Error = &e
	}
	return &out
}

func redactToolCallResult(r mcp.ToolCallResult) mcp.ToolCallResult {
	content := make([]mcp.ContentBlock, len(r.Content))
	for i, b := range r.Content {
		b.Text = redact.ToolOutput(b.Text)
		content[i] = b
	}
	r.Content = content
	return r
}

// redactToolCallMap redacts a tool call result decoded as a generic map: the
// text of its content blocks (including embedded resources), as in
// redactToolCallResult, and every other field (structuredContent, _meta ...)
// structurally. It reports false, and
// leaves r alone, if r is not a tool call result. r is not modified.
func redactToolCallMap(r map[string]interface{}) (map[string]interface{}, bool) {
	content, hasContent := r["content"].([]interface{})
	_, hasStructured := r["structuredContent"]
	if !hasContent && !hasStructured {
		return nil, false
	}
	m := make(map[string]interface{}, len(r))
	for k, v := range r {
		switch k {
		case "content", "isError":
			m[k] = v
		default:
			m[k] = redact.Value(v)
		}
	}
	if hasContent {
		blocks := make([]interface{}, len(content))
		for i, b := range content {
			blocks[i] = b
			if block, ok := b.(map[string]interface{}); ok {
				blocks[i] = redactContentBlock(block)
			}
		}
		m["content"] = blocks
	}
	return m, true
}

// redactContentBlock returns a content block with its text redacted: the
// text of a text block and the text of an embedded resource
// ({"type":"resource","resource":{"uri":...,"text":...}}). block is not
// modified.
func redactContentBlock(block map[string]interface{}) map[string]interface{} {
	text, hasText := block["text"].(string)
	res, hasRes := block["resource"].(map[string]interface{})
	resText, hasResText := res["text"].(string)
	if !hasText && !(hasRes && hasResText) {
		return block
	}
	nb := make(map[string]interface{}, len(block))
	for k, v := range block {
		nb[k] = v
	}
	if hasText {
		nb["text"] = redact.ToolOutput(text)
	}
	if hasRes && hasResText {
		nr := make(map[string]interface{}, len(res))
		for k, v := range res {
			nr[k] = v
		}
		nr["text"] = redact.ToolOutput(resText)
		nb["resource"] = nr
	}
	return nb
}

// decodeResult decodes a raw JSON result that is an object, keeping numbers
// exact.
func decodeResult(raw json.RawMessage) (map[string]interface{}, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]interface{}
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}
