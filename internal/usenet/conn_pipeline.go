package usenet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/yenc"
)

// errPipelineUnanswered identifies a command stranded behind an earlier failed
// response. It has not consumed its own article retry budget.
var errPipelineUnanswered = errors.New("nntp: pipeline reply not reached")

func (c *Conn) bodyPipelineStarted(ctx context.Context, ids []string, opt *yenc.DecodeOptions, started func(), deliver func(int, *yenc.Part, error)) {
	fail := func(from int, err error) {
		for i := from; i < len(ids); i++ {
			replyErr := err
			if i > from {
				replyErr = fmt.Errorf("%w: %w", errPipelineUnanswered, err)
			}
			deliver(i, nil, replyErr)
		}
	}
	if len(ids) == 0 {
		return
	}
	if len(ids) > 16 || c.inBody {
		fail(0, errors.New("nntp: invalid BODY pipeline window"))
		return
	}
	normalized := make([]string, len(ids))
	for i, id := range ids {
		var err error
		normalized[i], err = normalizeMessageID(id)
		if err != nil {
			fail(0, err)
			return
		}
	}
	end, err := c.begin(ctx, c.commandTimeout)
	if err != nil {
		fail(0, err)
		return
	}
	c.bodyStarted = false
	start := time.Now()
	for _, id := range normalized {
		if c.onCommand != nil {
			c.onCommand("BODY " + id)
		}
		if _, err = c.w.WriteString("BODY " + id + "\r\n"); err != nil {
			break
		}
	}
	if err == nil {
		err = c.w.Flush()
	}
	if err = end(err); err != nil {
		c.broken = true
		fail(0, err)
		return
	}
	if started != nil {
		started()
	}
	for i := range ids {
		end, err = c.begin(ctx, c.commandTimeout)
		if err != nil {
			c.broken = true
			fail(i, err)
			return
		}
		c.bodyStarted = false
		code, msg, err := c.readStatus()
		c.lastRTT = time.Since(start)
		if err != nil {
			c.broken = true
			fail(i, end(err))
			return
		}
		if code != 222 {
			err = end(&ProtocolError{Cmd: "BODY", Code: code, Msg: msg})
			// Unknown successful responses may have a multiline payload: never infer
			// framing. 400 explicitly announces connection closure.
			if code == 400 || code < 400 {
				c.broken = true
				fail(i, err)
				return
			}
			deliver(i, nil, err)
			continue
		}
		c.bodyStarted = true
		c.inBody = true
		br := &BodyReader{c: c, end: end}
		part, derr := decodeBody(br, opt)
		cerr := br.Close()
		if br.ioErr != nil {
			fail(i, br.ioErr)
			return
		}
		if cerr != nil {
			fail(i, cerr)
			return
		}
		if derr != nil {
			deliver(i, part, fmt.Errorf("%w: %s: %w", ErrCorrupt, ids[i], derr))
		} else {
			deliver(i, part, nil)
		}
	}
}
