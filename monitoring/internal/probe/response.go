package probe

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func (p *Prober) doRequest(client *http.Client, request *http.Request, result model.ProbeResult) model.ProbeResult {
	start := time.Now()
	var firstByte time.Time
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() { firstByte = time.Now() }}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	response, err := client.Do(request)
	if err != nil {
		applyLatency(&result, start, firstByte)
		result.Status = model.StatusError
		result.ErrorClass = classifyNetworkError(err)
		result.Message = networkErrorMessage(err)
		return result
	}
	defer response.Body.Close()
	_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
	applyLatency(&result, start, firstByte)
	statusCode := response.StatusCode
	result.StatusCode = &statusCode
	if readErr != nil {
		result.Status = model.StatusError
		result.ErrorClass = "read"
		result.Message = "读取上游响应失败"
		return result
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		result.Status = model.StatusOperational
		return result
	}
	result.Status = model.StatusFailed
	result.ErrorClass = "upstream"
	result.Message = responseMessage(response.StatusCode)
	return result
}

func applyLatency(result *model.ProbeResult, start, firstByte time.Time) {
	total := durationMs(time.Since(start))
	result.LatencyMs = &total
	if !firstByte.IsZero() {
		value := durationMs(firstByte.Sub(start))
		result.FirstByteMs = &value
	}
}

func durationMs(duration time.Duration) int {
	value := int(duration / time.Millisecond)
	if value < 1 {
		return 1
	}
	return value
}

func classifyNetworkError(err error) string {
	if errorsIsTimeout(err) {
		return "timeout"
	}
	return "network"
}

func networkErrorMessage(err error) string {
	if errorsIsTimeout(err) {
		return "上游请求超时"
	}
	return "上游请求失败"
}

func errorsIsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "timeout")
}

func responseMessage(status int) string {
	return fmt.Sprintf("上游返回 HTTP %d", status)
}
