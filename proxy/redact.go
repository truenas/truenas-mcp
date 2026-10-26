package proxy

import (
	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
)

// redactResponse returns resp with secrets removed from tool call content
// and error messages, the same redaction the server applies in
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
		if content, ok := r["content"].([]interface{}); ok {
			m := make(map[string]interface{}, len(r))
			for k, v := range r {
				m[k] = v
			}
			blocks := make([]interface{}, len(content))
			for i, b := range content {
				blocks[i] = b
				if block, ok := b.(map[string]interface{}); ok {
					if text, ok := block["text"].(string); ok {
						nb := make(map[string]interface{}, len(block))
						for k, v := range block {
							nb[k] = v
						}
						nb["text"] = redact.ToolOutput(text)
						blocks[i] = nb
					}
				}
			}
			m["content"] = blocks
			out.Result = m
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
