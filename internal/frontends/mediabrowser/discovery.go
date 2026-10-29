package mediabrowser

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"sync"
)

type discovery struct {
	info PublicInfo
	port int
	mu   sync.Mutex
	conn *net.UDPConn
	done chan struct{}
}

func (d *discovery) Start(ctx context.Context) error {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 7359})
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.conn = conn
	d.done = make(chan struct{})
	done := d.done
	d.mu.Unlock()
	go func() {
		defer close(done)
		buf := make([]byte, 1024)
		for {
			n, peer, e := conn.ReadFromUDP(buf)
			if e != nil {
				return
			}
			if string(buf[:n]) != "Who is JellyfinServer?" {
				continue
			}
			source, e := net.DialUDP("udp4", nil, peer)
			if e != nil {
				continue
			}
			local := source.LocalAddr().(*net.UDPAddr).IP
			_ = source.Close()
			address := net.JoinHostPort(local.String(), strconv.Itoa(d.port))
			b, _ := json.Marshal(map[string]string{"Address": "http://" + address, "Id": d.info.ID, "Name": d.info.ServerName, "EndpointAddress": local.String()})
			_, _ = conn.WriteToUDP(b, peer)
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = d.Close()
		case <-done:
		}
	}()
	return nil
}
func (d *discovery) Close() error {
	d.mu.Lock()
	conn, done := d.conn, d.done
	d.conn = nil
	d.mu.Unlock()
	if conn == nil {
		return nil
	}
	err := conn.Close()
	<-done
	return err
}
