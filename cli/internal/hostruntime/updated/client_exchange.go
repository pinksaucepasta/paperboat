package updated

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"reflect"
	"sync"
	"time"
)

type controlClientFailure struct {
	kind  error
	cause error
}

func (e controlClientFailure) Error() string {
	return "The local Paperboat updater request failed. Check `pb update status` before retrying."
}
func (e controlClientFailure) Unwrap() error         { return e.cause }
func (e controlClientFailure) Is(target error) bool  { return target == e.kind }
func (controlClientFailure) DiagnosticStage() string { return "control_request" }
func (controlClientFailure) DiagnosticCode() string  { return "control_request_failed" }

// The request owns its connection until decode and cancellation cleanup have
// both completed. Cancellation closes blocked pipe/socket I/O immediately.
func exchangeControl(ctx context.Context, connection net.Conn, request ControlRequest, timeout time.Duration, halfClose bool) (response ControlResponse, resultErr error) {
	var closeOnce sync.Once
	var closeErr error
	closeConnection := func() { closeOnce.Do(func() { closeErr = connection.Close() }) }
	cancelDone := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() { defer close(cancelDone); closeConnection() })
	defer func() {
		if !stopCancel() {
			<-cancelDone
		}
		closeConnection()
		if contextErr := ctx.Err(); contextErr != nil {
			if controlCancellationOnly(resultErr, contextErr) {
				resultErr = contextErr
			} else {
				resultErr = errors.Join(resultErr, contextErr)
			}
		}
		if closeErr != nil {
			resultErr = errors.Join(resultErr, closeErr)
		}
	}()
	deadline := time.Now().Add(timeout)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return ControlResponse{}, controlClientFailure{cause: err}
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return ControlResponse{}, controlClientFailure{cause: err}
	}
	if halfClose {
		closer, ok := connection.(interface{ CloseWrite() error })
		if !ok {
			return ControlResponse{}, ErrInvalidControl
		}
		if err := closer.CloseWrite(); err != nil {
			return ControlResponse{}, controlClientFailure{kind: ErrInvalidControl, cause: err}
		}
	}
	decoder := json.NewDecoder(io.LimitReader(connection, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return ControlResponse{}, controlClientFailure{kind: ErrInvalidControl, cause: err}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			err = ErrInvalidControl
		}
		return ControlResponse{}, controlClientFailure{kind: ErrInvalidControl, cause: err}
	}
	if response.Schema != ControlProtocolV1 || (response.Status != "ok" && response.Status != "error") || !validControlResponseError(response.Status, response.ErrorCode, response.ErrorMessage) {
		return ControlResponse{}, ErrInvalidControl
	}
	if response.ErrorCode != "" {
		return ControlResponse{}, &ControlError{Code: response.ErrorCode, Message: response.ErrorMessage}
	}
	return response, nil
}

func controlCancellationOnly(err, contextErr error) bool {
	for count := 0; err != nil && count < 16; count++ {
		value := reflect.ValueOf(err)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return false
		}
		if err == contextErr || err == net.ErrClosed || err == io.ErrClosedPipe || contextErr == context.DeadlineExceeded && err == os.ErrDeadlineExceeded {
			return true
		}
		wrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapper.Unwrap()
	}
	return err == nil
}
