package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestRetryDoesNotDowngradeUnknownDispatchToOffline(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "server.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	served := make(chan struct{})
	go func() {
		defer close(served)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		bufio.NewScanner(conn).Scan()
		listener.Close()
	}()
	req, _ := NewRequest("process.start", "original", map[string]string{"process_id": "original"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = (Client{Socket: socket}).Retry(ctx, req, nil, 3, time.Millisecond)
	<-served
	var wire *Error
	if !errors.As(err, &wire) || wire.Data.Kind != "transport_unknown" || wire.Data.OperationID != "original" {
		t.Fatal("uncertainty lost", err)
	}
}

func TestMutationMalformedReplyRemainsUnknown(t *testing.T) {
	for _, failure := range []string{"json", "identity", "result"} {
		t.Run(failure, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "server.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			served := make(chan struct{})
			go func() {
				defer close(served)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				scanner := bufio.NewScanner(conn)
				if !scanner.Scan() {
					return
				}
				var req Request
				if json.Unmarshal(scanner.Bytes(), &req) != nil {
					return
				}
				if failure == "json" {
					conn.Write([]byte("bad JSON\n"))
					return
				}
				resp := Response{JSONRPC: "2.0", Version: Version, ID: req.ID, Result: json.RawMessage(`"unexpected string"`)}
				if failure == "identity" {
					resp.ID = "another"
				}
				_ = json.NewEncoder(conn).Encode(resp)
			}()
			req, _ := NewRequest("process.stdin", "write-once", map[string]string{"process_id": "p"})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var result struct{ Written int }
			err = (Client{Socket: socket}).Call(ctx, req, &result)
			<-served
			var wire *Error
			if !errors.As(err, &wire) || wire.Data.Kind != "transport_unknown" {
				t.Fatal(err)
			}
		})
	}
}
