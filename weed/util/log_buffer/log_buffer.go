package log_buffer

import (
	"bytes"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"

	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/pb/filer_pb"
	"github.com/chrislusf/seaweedfs/weed/util"
)

const BufferSize = 4 * 1024 * 1024
const PreviousBufferCount = 3

type dataToFlush struct {
	startTime time.Time
	stopTime  time.Time
	data      *bytes.Buffer
}

type LogBuffer struct {
	prevBuffers   *SealedBuffers
	buf           []byte
	idx           []int
	pos           int
	startTime     time.Time
	stopTime      time.Time
	sizeBuf       []byte
	flushInterval time.Duration
	flushFn       func(startTime, stopTime time.Time, buf []byte)
	notifyFn      func()
	isStopping    bool
	flushChan     chan *dataToFlush
	sync.RWMutex
}

func NewLogBuffer(flushInterval time.Duration, flushFn func(startTime, stopTime time.Time, buf []byte), notifyFn func()) *LogBuffer {
	lb := &LogBuffer{
		prevBuffers:   newSealedBuffers(PreviousBufferCount),
		buf:           make([]byte, BufferSize),
		sizeBuf:       make([]byte, 4),
		flushInterval: flushInterval,
		flushFn:       flushFn,
		notifyFn:      notifyFn,
		flushChan:     make(chan *dataToFlush, 256),
	}
	go lb.loopFlush()
	go lb.loopInterval()
	return lb
}

func (m *LogBuffer) AddToBuffer(partitionKey, data []byte) {

	m.Lock()
	defer func() {
		m.Unlock()
		if m.notifyFn != nil {
			m.notifyFn()
		}
	}()

	// need to put the timestamp inside the lock
	ts := time.Now()
	logEntry := &filer_pb.LogEntry{
		TsNs:             ts.UnixNano(),
		PartitionKeyHash: util.HashToInt32(partitionKey),
		Data:             data,
	}

	logEntryData, _ := proto.Marshal(logEntry)

	size := len(logEntryData)

	if m.pos == 0 {
		m.startTime = ts
	}

	if m.startTime.Add(m.flushInterval).Before(ts) || len(m.buf)-m.pos < size+4 {
		m.flushChan <- m.copyToFlush()
		m.startTime = ts
		if len(m.buf) < size+4 {
			m.buf = make([]byte, 2*size+4)
		}
	}
	m.stopTime = ts

	m.idx = append(m.idx, m.pos)
	util.Uint32toBytes(m.sizeBuf, uint32(size))
	copy(m.buf[m.pos:m.pos+4], m.sizeBuf)
	copy(m.buf[m.pos+4:m.pos+4+size], logEntryData)
	m.pos += size + 4
}

func (m *LogBuffer) Shutdown() {
	if m.isStopping {
		return
	}
	m.isStopping = true
	m.Lock()
	toFlush := m.copyToFlush()
	m.Unlock()
	m.flushChan <- toFlush
	close(m.flushChan)
}

func (m *LogBuffer) loopFlush() {
	for d := range m.flushChan {
		if d != nil {
			m.flushFn(d.startTime, d.stopTime, d.data.Bytes())
			d.releaseMemory()
		}
	}
}

func (m *LogBuffer) loopInterval() {
	for !m.isStopping {
		m.Lock()
		toFlush := m.copyToFlush()
		m.Unlock()
		m.flushChan <- toFlush
		time.Sleep(m.flushInterval)
	}
}

func (m *LogBuffer) copyToFlush() *dataToFlush {

	if m.flushFn != nil && m.pos > 0 {
		// fmt.Printf("flush buffer %d pos %d empty space %d\n", len(m.buf), m.pos, len(m.buf)-m.pos)
		d := &dataToFlush{
			startTime: m.startTime,
			stopTime:  m.stopTime,
			data:      copiedBytes(m.buf[:m.pos]),
		}
		m.buf = m.prevBuffers.SealBuffer(m.startTime, m.stopTime, m.buf)
		m.pos = 0
		m.idx = m.idx[:0]
		return d
	}
	return nil
}

func (d *dataToFlush) releaseMemory() {
	d.data.Reset()
	bufferPool.Put(d.data)
}

func (m *LogBuffer) ReadFromBuffer(lastReadTime time.Time) (bufferCopy *bytes.Buffer) {
	m.RLock()
	defer m.RUnlock()

	// fmt.Printf("read from buffer: %v last stop time: %v\n", lastReadTime.UnixNano(), m.stopTime.UnixNano())

	if lastReadTime.Equal(m.stopTime) {
		return nil
	}
	if lastReadTime.After(m.stopTime) {
		// glog.Fatalf("unexpected last read time %v, older than latest %v", lastReadTime, m.stopTime)
		return nil
	}
	if lastReadTime.Before(m.startTime) {
		return copiedBytes(m.buf[:m.pos])
	}

	lastTs := lastReadTime.UnixNano()
	l, h := 0, len(m.idx)-1

	/*
		for i, pos := range m.idx {
			logEntry, ts := readTs(m.buf, pos)
			event := &filer_pb.SubscribeMetadataResponse{}
			proto.Unmarshal(logEntry.Data, event)
			entry := event.EventNotification.OldEntry
			if entry == nil {
				entry = event.EventNotification.NewEntry
			}
			fmt.Printf("entry %d ts: %v offset:%d dir:%s name:%s\n", i, time.Unix(0, ts), pos, event.Directory, entry.Name)
		}
		fmt.Printf("l=%d, h=%d\n", l, h)
	*/

	for l <= h {
		mid := (l + h) / 2
		pos := m.idx[mid]
		_, t := readTs(m.buf, pos)
		if t <= lastTs {
			l = mid + 1
		} else if lastTs < t {
			var prevT int64
			if mid > 0 {
				_, prevT = readTs(m.buf, m.idx[mid-1])
			}
			if prevT <= lastTs {
				// fmt.Printf("found l=%d, m-1=%d(ts=%d), m=%d(ts=%d), h=%d [%d, %d) \n", l, mid-1, prevT, mid, t, h, pos, m.pos)
				return copiedBytes(m.buf[pos:m.pos])
			}
			h = mid
		}
		// fmt.Printf("l=%d, h=%d\n", l, h)
	}

	// FIXME: this could be that the buffer has been flushed already
	return nil

}
func (m *LogBuffer) ReleaseMeory(b *bytes.Buffer) {
	b.Reset()
	bufferPool.Put(b)
}

var bufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

func copiedBytes(buf []byte) (copied *bytes.Buffer) {
	copied = bufferPool.Get().(*bytes.Buffer)
	copied.Write(buf)
	return
}

func readTs(buf []byte, pos int) (*filer_pb.LogEntry, int64) {

	size := util.BytesToUint32(buf[pos : pos+4])
	entryData := buf[pos+4 : pos+4+int(size)]
	logEntry := &filer_pb.LogEntry{}

	err := proto.Unmarshal(entryData, logEntry)
	if err != nil {
		glog.Fatalf("unexpected unmarshal filer_pb.LogEntry: %v", err)
	}
	return logEntry, logEntry.TsNs

}
