package main

import (
	contextpkg "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/sourcegraph/jsonrpc2"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

type Watcher struct {
	globPattern string `json:"globPattern"`
}

type Watchers struct {
	watchers []Watcher `json:"watchers"`
}

type Server struct {
	command string
	err     error
	cmd     *exec.Cmd
	conn    *jsonrpc2.Conn
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	plexer  *Plexer
}

var Notifications = map[string]bool{
	"window/logMessage":                true,
	"initialized":                      true,
	"textDocument/didOpen":             true,
	"workspace/didChangeConfiguration": true,
	"textDocument/didChange":           true,
}

type Plexer struct {
	conn   *jsonrpc2.Conn
	server *Server
}

func main() {
	server := Server{
		command: os.Args[1],
	}

	plexer := Plexer{server: &server}
	server.plexer = &plexer

	plexer.conn = jsonrpc2.NewConn(
		contextpkg.Background(),
		jsonrpc2.NewBufferedStream(&plexer, jsonrpc2.VSCodeObjectCodec{}),
		&plexer,
	)

	server.Start()

	if server.err != nil {
		fmt.Errorf("unable to start process %s\n", server.err)
		return
	}

	<-plexer.conn.DisconnectNotify()
	fmt.Println("Connection closed")

	server.cmd.Process.Kill()
}

func (s *Server) Start() {
	s.cmd = exec.Command(s.command)

	stdin, err := s.cmd.StdinPipe()
	if err != nil {
		s.err = err
		return
	}
	s.stdin = stdin

	stdout, err := s.cmd.StdoutPipe()
	if err != nil {
		s.err = err
		return
	}
	s.stdout = stdout

	stderr, err := s.cmd.StderrPipe()
	if err != nil {
		s.err = err
		return
	}

	go func() {
		io.Copy(os.Stderr, stderr)
	}()

	if err := s.cmd.Start(); err != nil {
		s.err = err
		return
	}

	s.conn = jsonrpc2.NewConn(
		contextpkg.Background(),
		jsonrpc2.NewBufferedStream(s, jsonrpc2.VSCodeObjectCodec{}),
		s,
	)
}

func (s *Server) Handle(ctx contextpkg.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	LSPDispatch(s.plexer.conn, ctx, conn, req)
}

func (s *Server) Read(p []byte) (int, error) {
	return s.stdout.Read(p)
}

func (s *Server) Write(p []byte) (int, error) {
	return s.stdin.Write(p)
}

func (s *Server) Close() error {
	return errors.Join(s.stdin.Close(), s.stdout.Close())
}

func (p *Plexer) Handle(ctx contextpkg.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	LSPDispatch(p.server.conn, ctx, conn, req)
}

func (*Plexer) Read(p []byte) (int, error) {
	return os.Stdin.Read(p)
}

func (*Plexer) Write(p []byte) (int, error) {
	return os.Stdout.Write(p)
}

func (*Plexer) Close() error {
	return errors.Join(os.Stdin.Close(), os.Stdout.Close())
}

func LSPDispatch(r *jsonrpc2.Conn, ctx contextpkg.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	fmt.Fprintf(os.Stderr, "<start %v\n", req.Method)
	if Notifications[req.Method] {
		fmt.Fprintf(os.Stderr, "<dispatch %v>\n", req.Method)
		r.Notify(ctx, req.Method, req.Params)
	} else if req.Method == "workspace/configuration" {
		var completionItems []interface{}

		fmt.Fprintf(os.Stderr, "<completion %v %v>\n", req.ID, req.Method)
		err := r.Call(ctx, req.Method, req.Params, &completionItems)

		fmt.Fprintf(os.Stderr, "</completion %v %v %v>\n", req.ID, req.Method, err)
		if err != nil {
			err := fmt.Errorf("error forwarding requests: %v", err)
			conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    jsonrpc2.CodeInternalError,
				Message: err.Error(),
			})
		} else {
			conn.Reply(ctx, req.ID, completionItems)
		}
	} else if req.Method == "client/registerCapability" {
		var completionItems []interface{}
		var params protocol.RegistrationParams

		json.Unmarshal(*req.Params, &params)

		for i := range params.Registrations {
			if params.Registrations[i].Method == "workspace/didChangeWatchedFiles" {
				params.Registrations[i].RegisterOptions = Watchers{
					watchers: []Watcher{
						Watcher{globPattern: "**/{tailwind,tailwind.config}.{js,cjs,ts,mjs}"},
						Watcher{globPattern: "**/{package-lock.json,yarn.lock,pnpm-lock.yaml}"},
						Watcher{globPattern: "**/*.{css,scss,sass,less,pcss}"},
					},
				}
			}
		}

		fmt.Fprintf(os.Stderr, "<register %v %v %v>\n", req.ID, req.Method, params)
		err := r.Call(ctx, req.Method, params, &completionItems)

		fmt.Fprintf(os.Stderr, "</register %v %v %v>\n", req.ID, req.Method, err)
		if err != nil {
			err := fmt.Errorf("error forwarding requests: %v", err)
			conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    jsonrpc2.CodeInternalError,
				Message: err.Error(),
			})
		} else {
			conn.Reply(ctx, req.ID, completionItems)
		}
	} else {
		var res map[string]interface{}

		fmt.Fprintf(os.Stderr, "<calling %v %v %v>\n", req.ID, req.Method, string(*req.Params))
		tctx, cancel := contextpkg.WithTimeout(contextpkg.Background(), time.Duration(time.Minute))
		err := r.Call(tctx, req.Method, req.Params, &res)
		cancel()

		fmt.Fprintf(os.Stderr, "</calling %v %v %v>\n", req.ID, req.Method, err)
		if err != nil {
			err := fmt.Errorf("error forwarding requests: %v", err)
			conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    jsonrpc2.CodeInternalError,
				Message: err.Error(),
			})
		} else {
			conn.Reply(ctx, req.ID, res)
		}
	}
}
