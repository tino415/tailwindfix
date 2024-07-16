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

type Client struct {
	cmd    *exec.Cmd
	conn   *jsonrpc2.Conn
	stdin  io.WriteCloser
	stdout io.ReadCloser
	state  *State
}

type Server struct {
	state *State
	conn  *jsonrpc2.Conn
}

type State struct {
	conn             *jsonrpc2.Conn
	cancelCodeAction contextpkg.CancelFunc
}

var Notifications = map[string]bool{
	"window/logMessage":                true,
	"initialized":                      true,
	"textDocument/didOpen":             true,
	"workspace/didChangeConfiguration": true,
	"textDocument/didChange":           true,
}

func main() {
	client := Client{
		cmd: exec.Command(os.Args[1]),
	}

	err := client.StartProcess()

	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to start Language server %v\n", err)
		return
	}

	server := Server{}

	client.StartConn()
	server.StartConn()

	client.state = &State{conn: server.conn}
	server.state = &State{conn: client.conn}

	<-server.conn.DisconnectNotify()
	fmt.Println("Connection closed")

	client.cmd.Process.Kill()
}

func (client *Client) StartConn() {
	client.conn = jsonrpc2.NewConn(
		contextpkg.Background(),
		jsonrpc2.NewBufferedStream(client, jsonrpc2.VSCodeObjectCodec{}),
		client,
	)
}

func (client *Client) StartProcess() error {
	stdin, err := client.cmd.StdinPipe()
	if err != nil {
		return err
	}
	client.stdin = stdin

	stdout, err := client.cmd.StdoutPipe()
	if err != nil {
		return err
	}
	client.stdout = stdout

	client.cmd.Stderr = os.Stderr

	if err := client.cmd.Start(); err != nil {
		return err
	}

	return nil
}

func (c *Client) Handle(ctx contextpkg.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	c.state.LSPDispatch(ctx, conn, req)
}

func (c *Client) Read(p []byte) (int, error) {
	return c.stdout.Read(p)
}

func (c *Client) Write(p []byte) (int, error) {
	return c.stdin.Write(p)
}

func (c *Client) Close() error {
	return errors.Join(c.stdin.Close(), c.stdout.Close())
}

func (server *Server) StartConn() {
	server.conn = jsonrpc2.NewConn(
		contextpkg.Background(),
		jsonrpc2.NewBufferedStream(server, jsonrpc2.VSCodeObjectCodec{}),
		server,
	)
}

func (s *Server) Handle(ctx contextpkg.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	s.state.LSPDispatch(ctx, conn, req)
}

func (*Server) Read(p []byte) (int, error) {
	return os.Stdin.Read(p)
}

func (*Server) Write(p []byte) (int, error) {
	return os.Stdout.Write(p)
}

func (*Server) Close() error {
	return errors.Join(os.Stdin.Close(), os.Stdout.Close())
}

func (s *State) LSPDispatch(ctx contextpkg.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	if Notifications[req.Method] {
		if req.Method == "textDocument/didChange" && s.cancelCodeAction != nil {
			s.cancelCodeAction()
			s.cancelCodeAction = nil
		}
		s.conn.Notify(ctx, req.Method, req.Params)
	} else if req.Method == "client/registerCapability" {
		var params protocol.RegistrationParams

		json.Unmarshal(*req.Params, &params)

		for i := range params.Registrations {
			if params.Registrations[i].Method == "workspace/didChangeWatchedFiles" {
				params.Registrations[i].RegisterOptions = Watchers{
					watchers: []Watcher{
						{globPattern: "**/{tailwind,tailwind.config}.{js,cjs,ts,mjs}"},
						{globPattern: "**/{package-lock.json,yarn.lock,pnpm-lock.yaml}"},
						{globPattern: "**/*.{css,scss,sass,less,pcss}"},
					},
				}
			}
		}

		s.Deliver(ctx, conn, req, &params)
	} else {
		s.Deliver(ctx, conn, req, req.Params)
	}
}

func (s *State) Deliver(ctx contextpkg.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request, params interface{}) {
	var res json.RawMessage
	var completed = make(chan error)

	tctx, cancel := contextpkg.WithTimeout(ctx, time.Duration(time.Second*20))
	defer cancel()

	call, err := s.conn.DispatchCall(tctx, req.Method, params)

	if err != nil {
		ReplyWithError(tctx, conn, req.ID, err)
	}

	go func() {
		s.cancelCodeAction = cancel
		completed <- call.Wait(tctx, &res)
	}()

	select {
	case err := <-completed:
		if err != nil {
			ReplyWithError(ctx, conn, req.ID, err)
		} else {
			conn.Reply(ctx, req.ID, res)
		}
	case <-tctx.Done():
		return
	}
}

func ReplyWithError(ctx contextpkg.Context, conn *jsonrpc2.Conn, id jsonrpc2.ID, err error) {
	errf := fmt.Errorf("error forwarding requests: %v", err)

	conn.ReplyWithError(ctx, id, &jsonrpc2.Error{
		Code:    jsonrpc2.CodeInternalError,
		Message: errf.Error(),
	})
}
