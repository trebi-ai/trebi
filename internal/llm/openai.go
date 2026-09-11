package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"
)

type openAI struct {
	client openai.Client
}

func newOpenAI(key, base string, headers map[string]string, hc *http.Client) (Provider, error) {
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
	return &openAI{client: openai.NewClient(opts...)}, nil
}

func (p *openAI) Chat(ctx context.Context, req Request) (*Response, error) {
	params := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(req.Model),
		Messages: openaiMessages(req),
	}
	if req.MaxTokens > 0 {
		params.MaxTokens = param.NewOpt(int64(req.MaxTokens))
	}
	if len(req.Tools) > 0 {
		params.Tools = openaiTools(req.Tools)
	}
	var extra []option.RequestOption
	for k, v := range req.Headers {
		extra = append(extra, option.WithHeader(k, v))
	}
	out, err := p.client.Chat.Completions.New(ctx, params, extra...)
	if err != nil {
		return nil, openaiErr(err)
	}
	return openaiResponse(out), nil
}

func openaiMessages(req Request) []openai.ChatCompletionMessageParamUnion {
	var msgs []openai.ChatCompletionMessageParamUnion
	if req.System != "" {
		msgs = append(msgs, openai.SystemMessage(req.System))
	}
	for _, m := range req.Messages {
		msgs = append(msgs, openaiOne(m))
	}
	return msgs
}

func openaiOne(m Message) openai.ChatCompletionMessageParamUnion {
	switch m.Role {
	case "assistant":
		asst := openai.ChatCompletionAssistantMessageParam{}
		if m.Content != "" {
			asst.Content.OfString = param.NewOpt(m.Content)
		}
		for _, tc := range m.ToolCalls {
			asst.ToolCalls = append(asst.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: tc.ID,
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name:      tc.Name,
						Arguments: string(tc.Arguments),
					},
				},
			})
		}
		return openai.ChatCompletionMessageParamUnion{OfAssistant: &asst}
	case "tool":
		return openai.ToolMessage(m.Content, m.ToolCallID)
	default:
		return openai.UserMessage(m.Content)
	}
}

func openaiTools(tools []Tool) []openai.ChatCompletionToolUnionParam {
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, t := range tools {
		out = append(out, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        t.Name,
			Description: param.NewOpt(t.Description),
			Parameters:  shared.FunctionParameters(schemaMap(t.Parameters)),
		}))
	}
	return out
}

func openaiResponse(out *openai.ChatCompletion) *Response {
	resp := &Response{Usage: Usage{
		Input:         out.Usage.PromptTokens,
		Output:        out.Usage.CompletionTokens,
		CacheRead:     out.Usage.PromptTokensDetails.CachedTokens,
		CacheCreation: out.Usage.PromptTokensDetails.CacheWriteTokens,
	}}
	if len(out.Choices) == 0 {
		resp.Stop = StopUnknown
		return resp
	}
	ch := out.Choices[0]
	resp.RawStop = ch.FinishReason
	resp.Stop = mapStop(ch.FinishReason)
	if ch.Message.Refusal != "" {
		resp.Stop = StopRefusal
		resp.Text = ch.Message.Refusal
	} else {
		resp.Text = ch.Message.Content
	}
	for _, tc := range ch.Message.ToolCalls {
		resp.ToolCalls = append(resp.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: json.RawMessage(tc.Function.Arguments),
		})
	}
	if len(resp.ToolCalls) > 0 && resp.Stop == StopEndTurn {
		resp.Stop = StopToolUse
	}
	return resp
}

func openaiErr(err error) error {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return wrapHTTPError(err, apiErr.StatusCode, apiErr.Message)
	}
	return err
}
