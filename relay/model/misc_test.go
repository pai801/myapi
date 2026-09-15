package model

import (
	"errors"
	"strings"
	"testing"
)

// TestProtocolConversionErrorContract 锁定共享协议错误类型（全局约束 §1.3）：
// 稳定机器码枚举、方向枚举、Error() 输出可定位信息、Unwrap() 可追溯底层原因。
func TestProtocolConversionErrorContract(t *testing.T) {
	wantCodes := []string{
		"invalid_source_json",
		"invalid_source_shape",
		"unsupported_mapping",
		"malformed_tool_call",
		"invalid_stream_event",
		"unsupported_output_item",
	}
	gotCodes := []string{
		CodeInvalidSourceJSON,
		CodeInvalidSourceShape,
		CodeUnsupportedMapping,
		CodeMalformedToolCall,
		CodeInvalidStreamEvent,
		CodeUnsupportedOutputItem,
	}
	for i, want := range wantCodes {
		if gotCodes[i] != want {
			t.Fatalf("stable machine code mismatch: want %q got %q", want, gotCodes[i])
		}
	}

	wantDirections := []string{
		"chat_request_to_responses",
		"responses_request_to_chat",
		"responses_response_to_chat",
		"chat_response_to_responses",
	}
	gotDirections := []string{
		DirectionChatRequestToResponses,
		DirectionResponsesRequestToChat,
		DirectionResponsesResponseToChat,
		DirectionChatResponseToResponses,
	}
	for i, want := range wantDirections {
		if gotDirections[i] != want {
			t.Fatalf("direction constant mismatch: want %q got %q", want, gotDirections[i])
		}
	}

	cause := errors.New("unexpected end of JSON input")
	err := &ProtocolConversionError{
		Code:      CodeInvalidSourceJSON,
		Direction: DirectionChatResponseToResponses,
		Path:      "messages[2].content[0].image_url.url",
		Cause:     cause,
	}

	msg := err.Error()
	for _, want := range []string{CodeInvalidSourceJSON, DirectionChatResponseToResponses, "messages[2].content[0].image_url.url", cause.Error()} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Error() must carry %q, got %q", want, msg)
		}
	}

	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is must trace through Unwrap, got %#v", err)
	}
	var target *ProtocolConversionError
	if !errors.As(error(err), &target) {
		t.Fatalf("errors.As must recover *ProtocolConversionError")
	}
	if target != err {
		t.Fatalf("errors.As returned a different value: %#v", target)
	}

	// Cause 为 nil 时 Unwrap 返回 nil，Error() 不得输出 "<nil>" 原因尾巴
	bare := &ProtocolConversionError{Code: CodeUnsupportedMapping, Direction: DirectionResponsesRequestToChat, Path: "tools[0]"}
	if errors.Unwrap(bare) != nil {
		t.Fatalf("expected nil cause to unwrap to nil, got %#v", errors.Unwrap(bare))
	}
	if strings.HasSuffix(bare.Error(), ": ") {
		t.Fatalf("Error() must not append an empty cause section, got %q", bare.Error())
	}
}

// TestErrorWithStatusCodeSemanticText 锁定共享语义文本拼接与 nil 安全（P2-4 谓词上移契约）。
func TestErrorWithStatusCodeSemanticText(t *testing.T) {
	tests := []struct {
		name string
		err  *ErrorWithStatusCode
		want string
	}{
		{
			name: "nil_receiver",
			err:  nil,
			want: "",
		},
		{
			name: "type_code_message_joined_lowercased_trimmed",
			err: &ErrorWithStatusCode{
				Error:      Error{Type: " Invalid_Request_Error ", Code: "Bad_Request", Message: "  Bad Body  "},
				StatusCode: 400,
			},
			want: "invalid_request_error | bad_request | bad body",
		},
		{
			name: "nil_code_stringified",
			err:  &ErrorWithStatusCode{Error: Error{Type: "upstream_error", Code: nil, Message: "boom"}},
			want: "upstream_error | <nil> | boom",
		},
		{
			name: "zero_value_code_stringified_nil",
			err:  &ErrorWithStatusCode{},
			want: " | <nil> | ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.SemanticText(); got != tt.want {
				t.Fatalf("SemanticText() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestErrorWithStatusCodeLooksLikeRequestShapeFailure 锁定请求形态类失败判定（P2-4）：
// 每个 marker 命中、nil 安全、真实上游失败（502/invalid_upstream_response/超时/限流）不误判；
// 并覆盖方案 A + P2-1 守卫：Type=upstream_error 且 Code 命中网关机器码全集（invalid_upstream_response
// 与六个协议转换机器码）时不凭 Message 文本豁免。
func TestErrorWithStatusCodeLooksLikeRequestShapeFailure(t *testing.T) {
	tests := []struct {
		name string
		err  *ErrorWithStatusCode
		want bool
	}{
		{name: "nil_receiver", err: nil, want: false},
		{
			name: "marker_invalid_request_error_in_type",
			err:  &ErrorWithStatusCode{Error: Error{Type: "invalid_request_error"}},
			want: true,
		},
		{
			name: "marker_invalid_request_in_message",
			err:  &ErrorWithStatusCode{Error: Error{Message: "Invalid request: missing field"}},
			want: true,
		},
		{
			name: "marker_invalid_input_in_message",
			err:  &ErrorWithStatusCode{Error: Error{Message: "invalid input: messages must be non-empty"}},
			want: true,
		},
		{
			name: "marker_invalid_parameter_in_message",
			err:  &ErrorWithStatusCode{Error: Error{Message: "invalid parameter: temperature"}},
			want: true,
		},
		{
			name: "marker_invalid_schema_in_message",
			err:  &ErrorWithStatusCode{Error: Error{Message: "invalid schema: tools[0].parameters"}},
			want: true,
		},
		{
			name: "marker_invalid_format_in_message",
			err:  &ErrorWithStatusCode{Error: Error{Message: "invalid format in tools"}},
			want: true,
		},
		{
			// 方案 A 守卫①：网关统一兜底包装（upstream_error/invalid_upstream_response）+
			// Message 内嵌 code=malformed_tool_call → 不得凭 marker 豁免（渠道/上游侧失败）。
			// 这是对 client-400 中途上报路径的刻意取舍：渠道计一次失败，换取上游数据完整性故障可被检出。
			name: "guard_wrapped_malformed_tool_call_returns_false",
			err: &ErrorWithStatusCode{Error: Error{
				Type:    "upstream_error",
				Code:    "invalid_upstream_response",
				Message: "protocol conversion failed: code=malformed_tool_call direction=responses_response_to_chat path=output_item.fc_1",
			}},
			want: false,
		},
		{
			// 守卫②：同一兜底包装下，Message 内嵌 invalid_request_error 同样不豁免（包装即渠道侧）。
			name: "guard_wrapped_invalid_request_error_returns_false",
			err: &ErrorWithStatusCode{
				Error: Error{
					Type:    "upstream_error",
					Code:    "invalid_upstream_response",
					Message: "protocol conversion failed: code=invalid_stream_event direction=responses_response_to_chat path=error: invalid_request_error: Invalid request body",
				},
				StatusCode: 502,
			},
			want: false,
		},
		{
			// 守卫③：未经兜底包装的真实客户端错误（400/invalid_request_error）仍判请求形态类。
			name: "unwrapped_real_client_error_still_request_shape",
			err: &ErrorWithStatusCode{
				Error:      Error{Type: "invalid_request_error", Code: "invalid_request_error", Message: "Invalid request body"},
				StatusCode: 400,
			},
			want: true,
		},
		{
			// 守卫④：保留机器码 invalid_stream_event 的包装现经网关机器码集守卫判为渠道/上游侧；
			// Message 内嵌 marker "request body"（marker 命中但被守卫拦下），故 false 的判定依据
			// 为集合命中而非 marker 缺失。若守卫收回为单值 invalid_upstream_response，本例将
			// 凭 marker 误判为 true（红）。
			name: "preserved_machine_code_invalid_stream_event_guard_hits",
			err: &ErrorWithStatusCode{
				Error:      Error{Type: "upstream_error", Code: CodeInvalidStreamEvent, Message: "protocol conversion failed: code=invalid_stream_event direction=responses_response_to_chat path=stream: upstream stream contained invalid request body fragment: <nil>"},
				StatusCode: 502,
			},
			want: false,
		},
		{
			// 守卫⑤：P2-1 已修。保留机器码 malformed_tool_call 的包装（外层 Code 非
			// invalid_upstream_response）旧行为凭 marker "malformed" 豁免为 true；集合扩全集后
			// 由守卫覆盖为 false，本例锁定该覆盖不回退。
			name: "preserved_machine_code_malformed_tool_call_guard_hits",
			err: &ErrorWithStatusCode{
				Error:      Error{Type: "upstream_error", Code: CodeMalformedToolCall, Message: "protocol conversion failed: code=malformed_tool_call direction=chat_response_to_responses path=choices[0].delta.tool_calls[0]"},
				StatusCode: 502,
			},
			want: false,
		},
		{
			// P2-1 集合覆盖：保留机器码 invalid_source_json 的包装，Message 内嵌 marker
			// "request body"（无守卫时凭 marker 豁免），现由集合守卫判为渠道/上游侧 → false。
			name: "preserved_machine_code_invalid_source_json_guard_hits",
			err: &ErrorWithStatusCode{
				Error:      Error{Type: "upstream_error", Code: CodeInvalidSourceJSON, Message: "protocol conversion failed: code=invalid_source_json direction=chat_response_to_responses path=request body"},
				StatusCode: 502,
			},
			want: false,
		},
		{
			// P2-1 集合覆盖：保留机器码 invalid_source_shape + marker "invalid input" → false。
			name: "preserved_machine_code_invalid_source_shape_guard_hits",
			err: &ErrorWithStatusCode{
				Error:      Error{Type: "upstream_error", Code: CodeInvalidSourceShape, Message: "protocol conversion failed: code=invalid_source_shape direction=chat_response_to_responses path=invalid input item"},
				StatusCode: 502,
			},
			want: false,
		},
		{
			// P2-1 集合覆盖：保留机器码 unsupported_mapping + marker "request schema" → false。
			name: "preserved_machine_code_unsupported_mapping_guard_hits",
			err: &ErrorWithStatusCode{
				Error:      Error{Type: "upstream_error", Code: CodeUnsupportedMapping, Message: "protocol conversion failed: code=unsupported_mapping direction=chat_request_to_responses path=request schema"},
				StatusCode: 502,
			},
			want: false,
		},
		{
			// P2-1 集合覆盖：保留机器码 unsupported_output_item + marker "invalid format" → false。
			name: "preserved_machine_code_unsupported_output_item_guard_hits",
			err: &ErrorWithStatusCode{
				Error:      Error{Type: "upstream_error", Code: CodeUnsupportedOutputItem, Message: "protocol conversion failed: code=unsupported_output_item direction=responses_response_to_chat path=invalid format output item"},
				StatusCode: 502,
			},
			want: false,
		},
		{
			name: "marker_unsupported_request_in_code",
			err:  &ErrorWithStatusCode{Error: Error{Code: "unsupported_request"}},
			want: true,
		},
		{
			name: "marker_request_body_in_message",
			err:  &ErrorWithStatusCode{Error: Error{Message: "request body too large"}},
			want: true,
		},
		{
			name: "marker_request_schema_in_message",
			err:  &ErrorWithStatusCode{Error: Error{Message: "request schema mismatch"}},
			want: true,
		},
		{
			name: "plain_502_invalid_upstream_response_not_misfire",
			err: &ErrorWithStatusCode{
				Error:      Error{Type: "upstream_error", Code: "invalid_upstream_response", Message: "bad gateway"},
				StatusCode: 502,
			},
			want: false,
		},
		{
			name: "timeout_not_misfire",
			err: &ErrorWithStatusCode{
				Error:      Error{Type: "upstream_error", Code: "timeout", Message: "context deadline exceeded"},
				StatusCode: 504,
			},
			want: false,
		},
		{
			name: "rate_limit_not_misfire",
			err: &ErrorWithStatusCode{
				Error:      Error{Type: "rate_limit_error", Code: "rate_limit_exceeded", Message: "Rate limit hit"},
				StatusCode: 429,
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.LooksLikeRequestShapeFailure(); got != tt.want {
				t.Fatalf("LooksLikeRequestShapeFailure() = %v, want %v", got, tt.want)
			}
		})
	}
}
