package clienthls

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aler9/gortsplib"
	"github.com/aler9/gortsplib/pkg/headers"
	"github.com/aler9/gortsplib/pkg/ringbuffer"
	"github.com/aler9/gortsplib/pkg/rtpaac"
	"github.com/aler9/gortsplib/pkg/rtph264"

	"github.com/aler9/rtsp-simple-server/internal/client"
	"github.com/aler9/rtsp-simple-server/internal/logger"
	"github.com/aler9/rtsp-simple-server/internal/stats"
)

const (
	pauseAfterAuthError = 2 * time.Second
)

type trackIDPayloadPair struct {
	trackID int
	buf     []byte
}

// PathMan is implemented by pathman.PathMan.
type PathMan interface {
	OnClientSetupPlay(client.SetupPlayReq)
}

// Parent is implemented by clientman.ClientMan.
type Parent interface {
	Log(logger.Level, string, ...interface{})
	OnClientClose(client.Client)
}

// Client is a HLS client.
type Client struct {
	readBufferCount int
	wg              *sync.WaitGroup
	stats           *stats.Stats
	pathName        string
	pathMan         PathMan
	parent          Parent

	ringBuffer *ringbuffer.RingBuffer

	// in
	terminate chan struct{}
}

// New allocates a Client.
func New(
	readBufferCount int,
	wg *sync.WaitGroup,
	stats *stats.Stats,
	pathName string,
	pathMan PathMan,
	parent Parent) *Client {

	c := &Client{
		readBufferCount: readBufferCount,
		wg:              wg,
		stats:           stats,
		pathName:        pathName,
		pathMan:         pathMan,
		parent:          parent,
		terminate:       make(chan struct{}),
	}

	atomic.AddInt64(c.stats.CountClients, 1)
	c.log(logger.Info, "connected (HLS)")

	c.wg.Add(1)
	go c.run()

	return c
}

// Close closes a Client.
func (c *Client) Close() {
	atomic.AddInt64(c.stats.CountClients, -1)
	close(c.terminate)
}

// IsClient implements client.Client.
func (c *Client) IsClient() {}

// IsSource implements path.source.
func (c *Client) IsSource() {}

func (c *Client) log(level logger.Level, format string, args ...interface{}) {
	c.parent.Log(level, "[client hls/%s] "+format, append([]interface{}{c.pathName}, args...)...)
}

func (c *Client) PathName() string {
	return c.pathName
}

func (c *Client) run() {
	defer c.wg.Done()
	defer c.log(logger.Info, "disconnected")

	var path client.Path
	var tracks gortsplib.Tracks

	var videoTrack *gortsplib.Track
	var h264SPS []byte
	var h264PPS []byte
	var h264Decoder *rtph264.Decoder
	var audioTrack *gortsplib.Track
	var aacConfig []byte
	var aacDecoder *rtpaac.Decoder

	err := func() error {
		resc := make(chan client.SetupPlayRes)
		c.pathMan.OnClientSetupPlay(client.SetupPlayReq{c, c.pathName, nil, resc}) //nolint:govet
		res := <-resc

		if res.Err != nil {
			if _, ok := res.Err.(client.ErrAuthCritical); ok {
				// wait some seconds to stop brute force attacks
				select {
				case <-time.After(pauseAfterAuthError):
				case <-c.terminate:
				}
			}
			return res.Err
		}

		path = res.Path
		tracks = res.Tracks

		return nil
	}()
	if err != nil {
		c.log(logger.Info, "ERR: %s", err)

		c.parent.OnClientClose(c)
		<-c.terminate
		return
	}

	err = func() error {
		for i, t := range tracks {
			if t.IsH264() {
				if videoTrack != nil {
					return fmt.Errorf("can't read track %d with RTMP: too many tracks", i+1)
				}
				videoTrack = t

				var err error
				h264SPS, h264PPS, err = t.ExtractDataH264()
				if err != nil {
					return err
				}

			} else if t.IsAAC() {
				if audioTrack != nil {
					return fmt.Errorf("can't read track %d with RTMP: too many tracks", i+1)
				}
				audioTrack = t

				var err error
				aacConfig, err = t.ExtractDataAAC()
				if err != nil {
					return err
				}
			}
		}

		if videoTrack == nil && audioTrack == nil {
			return fmt.Errorf("unable to find a video or audio track")
		}

		if videoTrack != nil {
			h264Decoder = rtph264.NewDecoder()
		}

		if audioTrack != nil {
			clockRate, _ := audioTrack.ClockRate()
			aacDecoder = rtpaac.NewDecoder(clockRate)
		}

		c.ringBuffer = ringbuffer.New(uint64(c.readBufferCount))

		return nil
	}()
	if err != nil {
		c.log(logger.Info, "ERR: %s", err)

		res := make(chan struct{})
		path.OnClientRemove(client.RemoveReq{c, res}) //nolint:govet
		<-res
		path = nil

		c.parent.OnClientClose(c)
		<-c.terminate
		return
	}

	resc := make(chan client.PlayRes)
	path.OnClientPlay(client.PlayReq{c, resc}) //nolint:govet
	<-resc

	c.log(logger.Info, "is reading from path '%s'", c.pathName)

	writerDone := make(chan error)
	go func() {
		writerDone <- func() error {
			for {
				data, ok := c.ringBuffer.Pull()
				if !ok {
					return fmt.Errorf("terminated")
				}
				pair := data.(trackIDPayloadPair)

				if videoTrack != nil && pair.trackID == videoTrack.ID {
					nts, err := h264Decoder.Decode(pair.buf)
					if err != nil {
						if err != rtph264.ErrMorePacketsNeeded {
							c.log(logger.Debug, "ERR while decoding video track: %v", err)
						}
						continue
					}

					for _, nt := range nts {
						fmt.Println("NAL", len(nt.NALU))
						/*if !videoInitialized {
							videoInitialized = true
							videoStartDTS = now
							videoPTS = nt.Timestamp
						}

						// aggregate NALUs by PTS
						if nt.Timestamp != videoPTS {
							pkt := av.Packet{
								Type: av.H264,
								Data: h264.FillNALUsAVCC(videoBuf),
								Time: now.Sub(videoStartDTS),
							}

							c.conn.NetConn().SetWriteDeadline(time.Now().Add(c.writeTimeout))
							err := c.conn.WritePacket(pkt)
							if err != nil {
								return err
							}

							videoBuf = nil
						}

						videoPTS = nt.Timestamp
						videoBuf = append(videoBuf, nt.NALU)*/
					}

				} else if audioTrack != nil && pair.trackID == audioTrack.ID {
					ats, err := aacDecoder.Decode(pair.buf)
					if err != nil {
						c.log(logger.Debug, "ERR while decoding audio track: %v", err)
						continue
					}

					for _, at := range ats {
						fmt.Println("A", len(at.AU))
						/*pkt := av.Packet{
							Type: av.AAC,
							Data: at.AU,
							Time: at.Timestamp,
						}

						c.conn.NetConn().SetWriteDeadline(time.Now().Add(c.writeTimeout))
						err := c.conn.WritePacket(pkt)
						if err != nil {
							return err
						}*/
					}
				}
			}
		}()
	}()

	select {
	case err := <-writerDone:
		c.log(logger.Info, "ERR: %s", err)

		res := make(chan struct{})
		path.OnClientRemove(client.RemoveReq{c, res}) //nolint:govet
		<-res
		path = nil

		c.parent.OnClientClose(c)
		<-c.terminate

	case <-c.terminate:
		res := make(chan struct{})
		path.OnClientRemove(client.RemoveReq{c, res}) //nolint:govet
		<-res

		c.ringBuffer.Close()
		<-writerDone
		path = nil
	}
}

// Authenticate performs an authentication.
func (c *Client) Authenticate(authMethods []headers.AuthMethod,
	pathName string, ips []interface{},
	user string, pass string, req interface{}) error {
	return nil
}

func (c *Client) OnFrame(trackID int, streamType gortsplib.StreamType, payload []byte) {
	if streamType == gortsplib.StreamTypeRTP {
		c.ringBuffer.Push(trackIDPayloadPair{trackID, payload})
	}
}
