package dap

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"opendrain/internal/action"
)

// Client speaks DAP over a pair of streams (e.g. a spawned `opendrain dap`
// process's stdin/stdout). It is deliberately tiny: a harness only needs
// Download() and the other method helpers.
type Client struct {
	in      io.Writer
	out     io.Reader
	mu      sync.Mutex
	seq     atomic.Int64
	pending map[string]chan *Response
}

func NewClient(in io.Writer, out io.Reader) *Client {
	c := &Client{in: in, out: out, pending: map[string]chan *Response{}}
	go c.readLoop()
	return c
}

func (c *Client) readLoop() {
	sc := bufio.NewScanner(c.out)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		resp, err := Decode(line)
		if err != nil {
			continue
		}
		c.mu.Lock()
		ch := c.pending[resp.ID]
		delete(c.pending, resp.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
}

// Call sends a request and waits for its response.
func (c *Client) Call(ctx context.Context, method string, params any) (*Response, error) {
	id := fmt.Sprintf("c-%d", c.seq.Add(1))
	p, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	req := Request{DAP: Version, ID: id, Method: method, Params: p}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	ch := make(chan *Response, 1)
	c.mu.Lock()
	c.pending[id] = ch
	_, werr := c.in.Write(b)
	c.mu.Unlock()
	if werr != nil {
		return nil, werr
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Download is the single method an agent should need. It returns a handle,
// never content.
func (c *Client) Download(ctx context.Context, in action.Intent) (*action.Handle, error) {
	resp, err := c.Call(ctx, "act", in)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return &action.Handle{Status: "failed", Error: resp.Error}, nil
	}
	data, _ := json.Marshal(resp.Result)
	var h action.Handle
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, err
	}
	return &h, nil
}
