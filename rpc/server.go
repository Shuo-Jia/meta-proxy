package rpc

import (
	"context"
	"io"
	"log"
	"net"
	"sync"
)

// Serve blocks until the connection shutdown.
func Serve() error {
	addr, err := net.ResolveTCPAddr("tcp", "0.0.0.0:34601")
	if err != nil {
		return err
	}
	listener, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return err
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Print("connection accept:", err.Error())
			continue
		}
		// TODO(wutao): add metric for connections number
		// TODO(wutao): add connections management

		// use one goroutine per connection
		go serveConn(conn)
	}
}

// conn is a network connection but abstracted as a ReadWriteCloser here in order to do mock test.
// The caller typically invokes serveConn in a go statement.
func serveConn(conn io.ReadWriteCloser) {
	dec := &requestDecoder{
		reader: conn,
	}
	enc := &responseEncoder{
		writer: conn,
	}

	// `ctx` is the root of all sub-tasks. It notifies the children to terminate
	//  if the connection encounters some error.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for {
		req, err := dec.readRequest()
		if err != nil {
			if err != io.EOF {
				log.Println(err)
				// TODO(wutao): send back rpc response for this error if the request is fully read
				continue
			}
			log.Printf("connection %s is closed", conn)
			break
		}

		// Asynchronously execute RPC handler in order to not block the connection reading.
		wg.Add(1)
		go func() {
			result := req.handler(ctx, req.args)
			err := enc.sendResponse(req, result)
			if err != nil {
				log.Println(err)
			}

			wg.Done()
		}()
	}

	// cancel the ongoing requests
	cancel()

	// This connection exits only when all children are terminated.
	wg.Wait()
}
