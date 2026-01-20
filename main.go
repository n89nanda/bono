package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/user/bono/prompts"
	"golang.org/x/term"
)

var (
	apiKey  string
	baseURL string
	model   string
	tools   []Tool
)

type Message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"tools,omitempty"`
}

type ChatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
}

func loadEnv() {
	f, err := os.Open(".env")
	if err != nil {
		return
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			os.Setenv(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
}

func getch() byte {
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return 0
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)
	b := make([]byte, 1)
	os.Stdin.Read(b)
	return b[0]
}

func chatCompletion(messages []Message) (*Message, error) {
	req := ChatRequest{Model: model, Messages: messages, Tools: tools}
	body, _ := json.Marshal(req)
	httpReq, _ := http.NewRequest("POST", baseURL+"/chat/completions", bytes.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("HTTP-Referer", "http://localhost")
	httpReq.Header.Set("X-Title", "Agent")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("api error (%d): %s", resp.StatusCode, b)
	}

	var chatResp ChatResponse
	json.NewDecoder(resp.Body).Decode(&chatResp)
	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}
	return &chatResp.Choices[0].Message, nil
}

func runTool(name string, args map[string]any) (string, bool) {
	marker := "● "
	var prompt, status, result string

	switch name {
	case "read_file":
		path := args["path"].(string)
		prompt = fmt.Sprintf("%sRead('%s')", marker, path)
		fmt.Print(prompt, " ")
		content, err := os.ReadFile(path)
		if err != nil {
			status = "fail: " + err.Error()
			result = ""
		} else {
			lines := len(strings.Split(string(content), "\n"))
			status = fmt.Sprintf("%d lines", lines)
			result = string(content)
		}
		fmt.Printf("\r%s => %s%s\n", prompt, status, strings.Repeat(" ", 20))
		return result, true

	case "write_file":
		path := args["path"].(string)
		content := args["content"].(string)
		lines := len(strings.Split(content, "\n"))
		prompt = fmt.Sprintf("%sWrite('%s', %d lines)", marker, path, lines)
		fmt.Print(prompt, " [Enter/Esc] ")
		if getch() == 0x1b {
			fmt.Println("=> cancelled")
			return "", false
		}
		err := os.WriteFile(path, []byte(content), 0644)
		if err != nil {
			status = "fail: " + err.Error()
		} else {
			status = "written"
		}
		result = "ok"
		fmt.Printf("\r%s => %s%s\n", prompt, status, strings.Repeat(" ", 20))
		return result, true

	case "edit_file":
		path := args["path"].(string)
		oldStr := args["old_string"].(string)
		newStr := args["new_string"].(string)
		replaceAll, _ := args["replace_all"].(bool)
		prompt = fmt.Sprintf("%sEdit('%s')", marker, path)
		fmt.Print(prompt, " [Enter/Esc] ")
		if getch() == 0x1b {
			fmt.Println("=> cancelled")
			return "", false
		}
		content, err := os.ReadFile(path)
		if err != nil {
			status = "fail: " + err.Error()
			fmt.Printf("\r%s => %s%s\n", prompt, status, strings.Repeat(" ", 20))
			return "", true
		}
		count := strings.Count(string(content), oldStr)
		if count == 0 {
			status = "fail: string not found"
			fmt.Printf("\r%s => %s%s\n", prompt, status, strings.Repeat(" ", 20))
			return "", true
		}
		if count > 1 && !replaceAll {
			status = fmt.Sprintf("fail: %d matches (use replace_all)", count)
			fmt.Printf("\r%s => %s%s\n", prompt, status, strings.Repeat(" ", 20))
			return "", true
		}
		var newContent string
		if replaceAll {
			newContent = strings.ReplaceAll(string(content), oldStr, newStr)
		} else {
			newContent = strings.Replace(string(content), oldStr, newStr, 1)
		}
		os.WriteFile(path, []byte(newContent), 0644)
		status = "ok"
		result = fmt.Sprintf("replaced %d occurrence(s)", count)
		fmt.Printf("\r%s => %s%s\n", prompt, status, strings.Repeat(" ", 20))
		return result, true

	default: // run_shell
		cmd := args["command"].(string)
		desc, _ := args["description"].(string)
		if desc == "" {
			desc = "(no description)"
		}
		safety, _ := args["safety"].(string)
		if safety == "" {
			safety = "modify"
		}
		prompt = fmt.Sprintf("%sBash('%s') # %s, %s", marker, cmd, desc, safety)
		fmt.Print(prompt, " [Enter/Esc] ")
		if getch() == 0x1b {
			fmt.Println("=> cancelled")
			return "", false
		}
		start := time.Now()
		out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
		elapsed := time.Since(start).Seconds()
		if err != nil {
			status = fmt.Sprintf("fail (%.1fs)", elapsed)
		} else {
			status = fmt.Sprintf("ok (%.1fs)", elapsed)
		}
		result = string(out)
		fmt.Printf("\r%s => %s%s\n", prompt, status, strings.Repeat(" ", 20))
		return result, true
	}
}

func main() {
	loadEnv()

	apiKey = os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		panic("OPENROUTER_API_KEY required")
	}
	baseURL = os.Getenv("BASE_URL")
	if baseURL == "" {
		baseURL = "https://openrouter.ai/api/v1"
	}
	model = os.Getenv("MODEL")
	if model == "" {
		model = "anthropic/claude-opus-4.5"
	}

	toolsData, err := os.ReadFile("tools.json")
	if err != nil {
		panic(err)
	}
	json.Unmarshal(toolsData, &tools)

	messages := []Message{{Role: "system", Content: prompts.System}}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT)
	go func() {
		<-sigChan
		fmt.Println("\nSee you later, alligator!")
		os.Exit(0)
	}()

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		input := scanner.Text()
		if input == "" {
			continue
		}

		messages = append(messages, Message{Role: "user", Content: input})

		for {
			msg, err := chatCompletion(messages)
			if err != nil {
				fmt.Println("Error:", err)
				break
			}

			msgJSON, _ := json.Marshal(msg)
			var msgMap map[string]any
			json.Unmarshal(msgJSON, &msgMap)
			messages = append(messages, Message{
				Role:      msg.Role,
				Content:   msg.Content,
				ToolCalls: msg.ToolCalls,
			})

			if len(msg.ToolCalls) == 0 {
				if msg.Content != nil {
					fmt.Println(msg.Content)
				}
				break
			}

			checkpoint := len(messages) - 1
			cancelled := false

			for _, tc := range msg.ToolCalls {
				var args map[string]any
				json.Unmarshal([]byte(tc.Function.Arguments), &args)
				result, ok := runTool(tc.Function.Name, args)
				if !ok {
					cancelled = true
					break
				}
				messages = append(messages, Message{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content:    result,
				})
			}

			if cancelled {
				messages = messages[:checkpoint]
				break
			}
		}
	}
}
