package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// printConfigFormats are the accepted --print-config values.
var printConfigFormats = []string{"claude-desktop", "claude-code", "cursor", "json"}

// clientSnippet renders the MCP client configuration for format. env holds
// settings only (host, user, file paths, TLS); it never holds the key.
func clientSnippet(format, command string, env map[string]string) (string, error) {
	server := map[string]interface{}{"command": command, "env": env}
	switch format {
	case "claude-desktop", "cursor":
		b, err := json.MarshalIndent(map[string]interface{}{"mcpServers": map[string]interface{}{"truenas": server}}, "", "  ")
		if err != nil {
			return "", err
		}
		return string(b) + "\n", nil
	case "json":
		b, err := json.MarshalIndent(server, "", "  ")
		if err != nil {
			return "", err
		}
		return string(b) + "\n", nil
	case "claude-code":
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("claude mcp add truenas")
		for _, k := range keys {
			b.WriteString(" --env " + shellQuote(k+"="+env[k]))
		}
		b.WriteString(" -- " + shellQuote(command) + "\n")
		return b.String(), nil
	}
	return "", fmt.Errorf("unknown --print-config format %q (want one of %s)", format, strings.Join(printConfigFormats, ", "))
}

// snippetHint says where a snippet goes.
func snippetHint(format string) string {
	switch format {
	case "claude-desktop":
		return "Merge into claude_desktop_config.json, then restart Claude Desktop:"
	case "cursor":
		return "Merge into ~/.cursor/mcp.json (or .cursor/mcp.json in a project):"
	case "claude-code":
		return "Run this to register the server with Claude Code:"
	}
	return "MCP server entry (command and env):"
}

// shellQuote quotes s for a POSIX shell when it holds anything beyond a
// safe set of characters.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-./:=@%+,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
