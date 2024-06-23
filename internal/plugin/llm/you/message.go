package you

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/bincooo/chatgpt-adapter/internal/common"
	"github.com/bincooo/chatgpt-adapter/internal/gin.handler/response"
	"github.com/bincooo/chatgpt-adapter/internal/vars"
	"github.com/bincooo/chatgpt-adapter/logger"
	"github.com/bincooo/chatgpt-adapter/pkg"
	"github.com/bincooo/you.com"
	"github.com/gin-gonic/gin"
	"strings"
	"time"
)

const ginTokens = "__tokens__"

func waitMessage(ch chan string, cancel func(str string) bool) (content string, err error) {

	for {
		message, ok := <-ch
		if !ok {
			break
		}

		if strings.HasPrefix(message, "error:") {
			return "", logger.WarpError(errors.New(message[6:]))
		}

		if strings.HasPrefix(message, "limits:") {
			continue
		}

		if len(message) > 0 {
			content += message
			if cancel != nil && cancel(content) {
				return content, nil
			}
		}
	}

	return content, nil
}

func waitResponse(ctx *gin.Context, matchers []common.Matcher, cancel chan error, ch chan string, sse bool) (content string) {
	var (
		created = time.Now().Unix()
		tokens  = ctx.GetInt(ginTokens)
	)

	logger.Info("waitResponse ...")
	for {
		select {
		case err := <-cancel:
			if err != nil {
				logger.Error(err)
				if response.NotSSEHeader(ctx) {
					response.Error(ctx, -1, err)
				}
				return
			}
			goto label
		default:
			message, ok := <-ch
			if !ok {
				goto label
			}

			if strings.HasPrefix(message, "error:") {
				logger.Error(message[6:])
				if response.NotSSEHeader(ctx) {
					response.Error(ctx, -1, message[6:])
				}
				return
			}

			if strings.HasPrefix(message, "limits:") {
				continue
			}

			var raw = message
			logger.Debug("----- raw -----")
			logger.Debug(raw)
			raw = common.ExecMatchers(matchers, raw)
			if len(raw) == 0 {
				continue
			}

			if sse {
				response.SSEResponse(ctx, Model, raw, created)
			}
			content += raw
		}
	}

label:
	if content == "" && response.NotSSEHeader(ctx) {
		return
	}

	ctx.Set(vars.GinCompletionUsage, common.CalcUsageTokens(content, tokens))
	if !sse {
		response.Response(ctx, Model, content)
	} else {
		response.SSEResponse(ctx, Model, "[DONE]", created)
	}

	return
}

func mergeMessages(completion pkg.ChatCompletion) (pMessages []you.Message, text string, tokens int, err error) {
	var messages = completion.Messages
	condition := func(expr string) string {
		switch expr {
		case "assistant", "end":
			return expr
		default:
			return "user"
		}
	}

	// 合并历史对话
	iterator := func(opts struct {
		Previous string
		Next     string
		Message  map[string]string
		Buffer   *bytes.Buffer
		Initial  func() pkg.Keyv[interface{}]
	}) (result []map[string]string, err error) {
		role := opts.Message["role"]
		tokens += common.CalcTokens(opts.Message["content"])
		if condition(role) == condition(opts.Next) {
			// cache buffer
			if role == "function" || role == "tool" {
				opts.Buffer.WriteString(fmt.Sprintf("这是内置工具的返回结果: (%s)\n\n##\n%s\n##", opts.Message["name"], opts.Message["content"]))
				return
			}

			prefix := ""
			if condition(role) == "user" {
				prefix = "Human： "
			}
			opts.Buffer.WriteString(prefix + opts.Message["content"])
			return
		}

		defer opts.Buffer.Reset()
		prefix := ""
		if condition(role) == "user" {
			prefix = "Human： "
		}
		opts.Buffer.WriteString(prefix + opts.Message["content"])
		result = append(result, map[string]string{
			"role":    condition(role),
			"content": opts.Buffer.String(),
		})
		return
	}

	newMessages, err := common.TextMessageCombiner(messages, iterator)
	if err != nil {
		err = logger.WarpError(err)
		return
	}

	text = "continue"
	is32 := false
	// 获取最后一条用户消息
	if tokens < 32*1000 {
		messageL := len(newMessages)
		message := newMessages[messageL-1]
		if message["role"] == "user" {
			newMessages = newMessages[:messageL-1]
			text = strings.TrimSpace(message["content"])
			text = strings.TrimLeft(text, "Human： ")
			messageL -= 1
		}
		is32 = true
	}

	// 理论上合并后的上下文不存在相邻的相同消息
	pos := 0
	messageL := len(newMessages)
	for {
		if pos >= messageL-1 {
			break
		}

		newMessage := you.Message{}
		message := newMessages[pos]
		if message["role"] == "user" {
			newMessage.Question = message["content"]
		}

		pos++
		if pos >= messageL-1 {
			pMessages = append(pMessages, newMessage)
			break
		}

		if message["role"] == "assistant" {
			prefix := ""
			if !is32 {
				prefix = "Assistant： "
			}
			newMessage.Question = prefix + message["content"]
		}
		pMessages = append(pMessages, newMessage)
		pos++
		break
	}
	return
}
