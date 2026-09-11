package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

type anthropicClient struct {
	client    anthropic.Client
	webSearch bool
}

func newAnthropic(key, base string, headers map[string]string, webSearch bool, hc *http.Client) (Provider, error) {
	opts := []option.RequestOption{
		option.WithAPIKey(key),
		option.WithBaseURL(base),
		option.WithMaxRetries(0),
	}
	if hc != nil {
		opts = append(opts, option.WithHTTPClient(hc))
	}
	for k, v := range headers {
		opts = append(opts, option.WithHeader(k, v))
	}
	return &anthropicClient{client: anthropic.NewClient(opts...), webSearch: webSearch}, nil
}

func (p *anthropicClient) Chat(ctx context.Context, req Request) (*Response, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: int64(req.MaxTokens),
		Messages:  anthropicMessages(req.Messages),
		Tools:     anthropicTools(req.Tools, p.webSearch),
	}
	if req.MaxTokens <= 0 {
		params.MaxTokens = 8192
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	var extra []option.RequestOption
	for k, v := range req.Headers {
		extra = append(extra, option.WithHeader(k, v))
	}
	msg, err := p.client.Messages.New(ctx, params, extra...)
	if err != nil {
		return nil, anthropicErr(err)
	}
	return anthropicResponse(msg), nil
}

func anthropicMessages(in []Message) []anthropic.MessageParam {
	var out []anthropic.MessageParam
	var pending []anthropic.ContentBlockParamUnion
	flushUser := func() {
		if len(pending) == 0 {
			return
		}
		out = append(out, anthropic.NewUserMessage(pending...))
		pending = nil
	}
	for _, m := range in {
		switch m.Role {
		case "assistant":
			flushUser()
			out = append(out, anthropicAssistant(m))
		case "tool":
			pending = append(pending, anthropic.NewToolResultBlock(m.ToolCallID, m.Content, false))
		default:
			flushUser()
			pending = append(pending, anthropic.NewTextBlock(m.Content))
		}
	}
	flushUser()
	return out
}

func anthropicAssistant(m Message) anthropic.MessageParam {
	var blocks []anthropic.ContentBlockParamUnion
	if m.Content != "" {
		blocks = append(blocks, anthropic.NewTextBlock(m.Content))
	}
	for _, tc := range m.ToolCalls {
		var input any
		if len(tc.Arguments) > 0 {
			_ = json.Unmarshal(tc.Arguments, &input)
		}
		blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, input, tc.Name))
	}
	if len(blocks) == 0 {
		blocks = append(blocks, anthropic.NewTextBlock(""))
	}
	return anthropic.NewAssistantMessage(blocks...)
}

func anthropicTools(tools []Tool, webSearch bool) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(tools)+1)
	for _, t := range tools {
		schema := schemaMap(t.Parameters)
		props, _ := schema["properties"]
		var required []string
		if r, ok := schema["required"].([]any); ok {
			for _, v := range r {
				if s, ok := v.(string); ok {
					required = append(required, s)
				}
			}
		} else if r, ok := schema["required"].([]string); ok {
			required = r
		}
		out = append(out, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        t.Name,
				Description: param.NewOpt(t.Description),
				InputSchema: anthropic.ToolInputSchemaParam{Properties: props, Required: required},
			},
		})
	}
	if webSearch {
		out = append(out, anthropic.ToolUnionParam{
			OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{},
		})
	}
	return out
}

func anthropicResponse(msg *anthropic.Message) *Response {
	resp := &Response{
		RawStop: string(msg.StopReason),
		Stop:    mapStop(string(msg.StopReason)),
		Usage: Usage{
			Input:         msg.Usage.InputTokens,
			Output:        msg.Usage.OutputTokens,
			CacheRead:     msg.Usage.CacheReadInputTokens,
			CacheCreation: msg.Usage.CacheCreationInputTokens,
		},
	}
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			resp.Text += b.Text
		case "tool_use":
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID:        b.ID,
				Name:      b.Name,
				Arguments: b.Input,
			})
		}
	}
	if len(resp.ToolCalls) > 0 && resp.Stop == StopEndTurn {
		resp.Stop = StopToolUse
	}
	return resp
}

func anthropicErr(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return wrapHTTPError(err, apiErr.StatusCode, apiErr.Error())
	}
	return err
}
