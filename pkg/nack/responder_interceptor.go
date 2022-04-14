package nack

import (
	"log"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// ResponderInterceptorFactory is a interceptor.Factory for a ResponderInterceptor
type ResponderInterceptorFactory struct {
	pli  *int
	opts []ResponderOption
}

// NewInterceptor constructs a new ResponderInterceptor
func (r *ResponderInterceptorFactory) NewInterceptor(id string) (interceptor.Interceptor, error) {
	i := &ResponderInterceptor{
		size:      8192,
		log:       logging.NewDefaultLoggerFactory().NewLogger("nack_responder"),
		streams:   map[uint32]*localStream{},
		packetMan: newPacketManager(),
		keyFrame:  []uint16{},
		pli:       r.pli,
	}

	for _, opt := range r.opts {
		if err := opt(i); err != nil {
			return nil, err
		}
	}

	if _, err := newSendBuffer(i.size); err != nil {
		return nil, err
	}

	return i, nil
}

// ResponderInterceptor responds to nack feedback messages
type ResponderInterceptor struct {
	interceptor.NoOp
	size      uint16
	log       logging.LeveledLogger
	packetMan *packetManager
	keyFrame  []uint16
	streams   map[uint32]*localStream
	streamsMu sync.Mutex
	pli       *int
}

type localStream struct {
	sendBuffer *sendBuffer
	rtpWriter  interceptor.RTPWriter
}

type Counter struct {
	rate  int           //计数周期内最多允许的请求数
	begin time.Time     //计数开始时间
	cycle time.Duration //计数周期
	count int           //计数周期内累计收到的请求数
	lock  sync.Mutex
}

func (l *Counter) Allow() bool {
	l.lock.Lock()
	defer l.lock.Unlock()

	log.Println("pli control ...")

	if l.count == l.rate-1 {
		now := time.Now()
		if now.Sub(l.begin) >= l.cycle {
			//速度允许范围内， 重置计数器
			l.Reset(now)
			return true
		} else {
			return false
		}
	} else {
		//没有达到速率限制，计数加1
		l.count++
		return true
	}
}

func (l *Counter) Set(r int, cycle time.Duration) {
	l.rate = r
	l.begin = time.Now()
	l.cycle = cycle
	l.count = 0
}

func (l *Counter) Reset(t time.Time) {
	l.begin = t
	l.count = 0
}

// NewResponderInterceptor returns a new ResponderInterceptorFactor
func NewResponderInterceptor(pli *int, opts ...ResponderOption) (*ResponderInterceptorFactory, error) {
	return &ResponderInterceptorFactory{pli: pli, opts: opts}, nil
}

// BindRTCPReader lets you modify any incoming RTCP packets. It is called once per sender/receiver, however this might
// change in the future. The returned method will be called once per packet batch.
func (n *ResponderInterceptor) BindRTCPReader(reader interceptor.RTCPReader) interceptor.RTCPReader {
	// num := 0

	var lr Counter
	lr.Set(5, time.Second) // 1s内最多请求5次
	return interceptor.RTCPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		i, attr, err := reader.Read(b, a)
		if err != nil {
			return 0, nil, err
		}
		// log.Println("responder interceptor rtcp reader: ", i)

		if attr == nil {
			attr = make(interceptor.Attributes)
		}
		pkts, err := attr.GetRTCPPackets(b[:i])
		if err != nil {
			return 0, nil, err
		}
		for _, rtcpPacket := range pkts {
			//nack:RTP包重发
			nack, nackStatus := rtcpPacket.(*rtcp.TransportLayerNack)
			if nackStatus {
				log.Println("fire nack!")
				go n.resendPackets(nack)
			}
			//pli:重发关键帧
			_, pliStatus := rtcpPacket.(*rtcp.PictureLossIndication)
			if pliStatus {
				log.Println("pli success!")
				if !lr.Allow() {
					log.Println("fire pli!")
					*n.pli = 1
				}
			}

		}

		return i, attr, err
	})
}

// BindLocalStream lets you modify any outgoing RTP packets. It is called once for per LocalStream. The returned method
// will be called once per rtp packet.
func (n *ResponderInterceptor) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	if !streamSupportNack(info) {
		return writer
	}

	// error is already checked in NewGeneratorInterceptor
	sendBuffer, _ := newSendBuffer(n.size)
	n.streamsMu.Lock()
	n.streams[info.SSRC] = &localStream{sendBuffer: sendBuffer, rtpWriter: writer}
	n.streamsMu.Unlock()
	// flag := 0

	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		pkt, err := n.packetMan.NewPacket(header, payload)
		if err != nil {
			return 0, err
		}
		// //存储关键帧
		// if header.ExtensionProfile == 1 {
		// 	if flag == 1 { //关键帧第一个RTP包,清空上一关键帧
		// 		n.keyFrame = n.keyFrame[:0]
		// 		flag = 0
		// 	}
		// 	if header.Marker { //本关键帧结束
		// 		flag = 1
		// 		log.Println("IDR SequenceNumber: ", header.SequenceNumber, len(n.keyFrame))
		// 	}
		// 	n.keyFrame = append(n.keyFrame, header.SequenceNumber)
		// }
		sendBuffer.add(pkt)
		// log.Println("BindLocalStream RTPWriter success: ")
		return writer.Write(header, payload, attributes)
	})
}

// UnbindLocalStream is called when the Stream is removed. It can be used to clean up any data related to that track.
func (n *ResponderInterceptor) UnbindLocalStream(info *interceptor.StreamInfo) {
	n.streamsMu.Lock()
	delete(n.streams, info.SSRC)
	n.streamsMu.Unlock()
}

func (n *ResponderInterceptor) resendPackets(nack *rtcp.TransportLayerNack) {
	n.streamsMu.Lock()
	stream, ok := n.streams[nack.MediaSSRC]
	n.streamsMu.Unlock()
	if !ok {
		return
	}

	for i := range nack.Nacks {
		nack.Nacks[i].Range(func(seq uint16) bool {
			if p := stream.sendBuffer.get(seq); p != nil {
				if _, err := stream.rtpWriter.Write(p.Header(), p.Payload(), interceptor.Attributes{}); err != nil {
					n.log.Warnf("failed resending nacked packet: %+v", err)
				}
				p.Release()
			}

			return true
		})
	}
}

// func (n *ResponderInterceptor) resendKeyFramePackets(pli *rtcp.PictureLossIndication) {
// 	n.streamsMu.Lock()
// 	stream, ok := n.streams[pli.MediaSSRC]
// 	n.streamsMu.Unlock()
// 	if !ok {
// 		return
// 	}
// 	for _, ssrc := range n.keyFrame {
// 		if p := stream.sendBuffer.get(ssrc); p != nil {
// 			if _, err := stream.rtpWriter.Write(p.Header(), p.Payload(), interceptor.Attributes{}); err != nil {
// 				n.log.Warnf("failed resending pli packet: %+v", err)
// 			}
// 			p.Release()
// 		}
// 	}
// }
