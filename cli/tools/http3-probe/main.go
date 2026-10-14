package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/quic-go/quic-go/http3"
)

func main() {
	exitCode, err := run(os.Args[1:])
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "http3-probe:", err)
	}
	os.Exit(exitCode)
}

type probeFailure struct {
	message string
	cause   error
}

func (e probeFailure) Error() string { return e.message }

func (e probeFailure) Unwrap() error { return e.cause }

func run(args []string) (int, error) {
	if len(args) < 1 || len(args) > 2 || len(args) == 2 && args[1] != "post" && args[1] != "pipe" {
		return 2, errors.New("usage: http3-probe URL [post|pipe]")
	}
	if err := probe(args); err != nil {
		return 1, err
	}
	return 0, nil
}

func probe(args []string) (probeErr error) {
	transport := &http3.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13}}
	defer func() {
		if err := transport.Close(); err != nil {
			probeErr = probeFailure{message: "HTTP/3 transport cleanup failed", cause: errors.Join(probeErr, err)}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	method := http.MethodGet
	var body *bytes.Reader
	if len(args) == 2 {
		method = http.MethodPost
		if args[1] == "pipe" {
			reader, writer := io.Pipe()
			writerDone := make(chan pipeWriterResult, 1)
			go func() {
				var result pipeWriterResult
				_, result.writeErrors[0] = writer.Write([]byte{'P', 'B', 'P', 'R', 1})
				_, result.writeErrors[1] = writer.Write([]byte("x"))
				result.closeError = writer.Close()
				writerDone <- result
			}()
			defer func() {
				if err := finishProbePipe(reader, writerDone); err != nil {
					probeErr = probeFailure{message: "HTTP/3 request body cleanup failed", cause: errors.Join(probeErr, err)}
				}
			}()
			requestBody := io.MultiReader(bytes.NewReader([]byte{'P', 'B', 'P', 'R', 1}), reader)
			request, err := http.NewRequestWithContext(ctx, method, args[0], requestBody)
			if err != nil {
				return probeFailure{message: "HTTP/3 request URL is invalid", cause: err}
			}
			request.Header.Set("Content-Type", "application/octet-stream")
			request.Header.Set("X-Paperboat-Connector-Admission", "invalid")
			started := time.Now()
			response, err := transport.RoundTrip(request)
			if err != nil {
				return probeFailure{message: "HTTP/3 probe request failed", cause: err}
			}
			return reportProbeResponse(response, started)
		}
		body = bytes.NewReader([]byte{'P', 'B', 'P', 'R', 1})
	} else {
		body = bytes.NewReader(nil)
	}
	request, err := http.NewRequestWithContext(ctx, method, args[0], body)
	if err != nil {
		return probeFailure{message: "HTTP/3 request URL is invalid", cause: err}
	}
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/octet-stream")
		request.Header.Set("X-Paperboat-Connector-Admission", "invalid")
	}
	started := time.Now()
	response, err := transport.RoundTrip(request)
	if err != nil {
		return probeFailure{message: "HTTP/3 probe request failed", cause: err}
	}
	return reportProbeResponse(response, started)
}

type pipeWriterResult struct {
	writeErrors [2]error
	closeError  error
}

func finishProbePipe(reader *io.PipeReader, writerDone <-chan pipeWriterResult) error {
	readerCloseErr := reader.Close()
	writerResult := <-writerDone
	return errors.Join(
		readerCloseErr,
		unexpectedPipeWriterError(writerResult.writeErrors[0]),
		unexpectedPipeWriterError(writerResult.writeErrors[1]),
		unexpectedPipeWriterError(writerResult.closeError),
	)
}

func unexpectedPipeWriterError(err error) error {
	if err == io.ErrClosedPipe {
		return nil
	}
	return err
}

func reportProbeResponse(response *http.Response, started time.Time) error {
	duration := time.Since(started)
	if err := response.Body.Close(); err != nil {
		return probeFailure{message: "HTTP/3 response body cleanup failed", cause: err}
	}
	if _, err := fmt.Printf("duration=%s protocol=%s status=%d\n", duration, response.Proto, response.StatusCode); err != nil {
		return probeFailure{message: "HTTP/3 probe result could not be written", cause: err}
	}
	return nil
}
