package logworker


import (
	"os"
	"time"
	"strconv"
	"fmt"
	"sync"
	"runtime"
	"sync/atomic"
	"strings"
	"github.com/marpaia/graphite-golang"
)

type Config struct {
	LoggerAddress      string
	StatsAddress       string
	Debug              int
	LogDir             string
	NumWorkers         int
	ByteBufferCapacity int
	EnableSSL          int
	EnableStats        int
	CookieDomain       string
	GenerateUDID		int
	CurrentLogFileHandle *os.File
	CurrentLogFile	    string
	ForceFsync      int
	EnableGraphite int
	GraphiteHost string
	GraphitePort int
}

type LogWorker struct {
	FileRoot             string
	buffer               []byte
	position             int
	RequestsHandled      int64
	CurrMinRequestSize   int32
	CurrMaxRequestSize   int32
}

type Stats struct {
	ProcessStartTime       int64 	`json:"omitempty"`
	CurrentProcessUptime	int64	`json:"current_uptime_seconds"`
	TotalRequestsServed    uint64 	`json:"total_requests_served"`
	NumWorkers 				int 	`json:"num_log_workers"`
	CpuStats               map[string]uint64 `json:"omitempty"`
	CurrentLoad            float32 `json:"curr_cpu_load"`
	CurrentProcessMemUsage int32 	`json:"curr_process_mem_usage"`
	CurrMaxRequestSize     int32	`json:"max_request_size"`
	CurrMinRequestSize     int32	`json:"min_request_size"`
	Timer                  *time.Timer `json:"omitempty"`
	PrevRequestsServed     uint64   `json:"omitempty"`
	RPS                    int32    `json:"rps"`
	CpuUsagePercentage		float32 `json:"cpu_usage"`
	GraphiteConn		*graphite.Graphite
	GraphiteConnError	bool
}

type LogEntry []byte


var (
	now          = time.Now()
	//channel      = make(chan []byte, 10000) // 6144-1 number of log events can be in the channel before it blocks
	//pending_write_channel      = make(chan LogEntry, 10000) // 10000-1 number of pending write events can be in the channel before it blocks
	Debug = 0
	ByteBufferCapacity int64
	mutexRPS = &sync.Mutex{}
	mutexIncr = &sync.Mutex{}
	mutexWrite = &sync.Mutex{}
	mutexCreate = &sync.Mutex{}
	LoggerConfig = NewConfig()
	LoggerStats = NewStats(LoggerConfig)
)

func NewStats(c *Config) *Stats {

	if c.EnableGraphite == 1 {
		fmt.Println("Graphite Enabled!")
		gr, err := graphite.NewGraphite(c.GraphiteHost, c.GraphitePort)
		if err != nil {
			return &Stats{ProcessStartTime: time.Now().Unix(), GraphiteConnError: true}
		}
		return &Stats{ProcessStartTime: time.Now().Unix(), GraphiteConn: gr}
	}
	return &Stats{ProcessStartTime: time.Now().Unix()}
}

func NewConfig() (c *Config) {
	return &Config{}
}

func Log(event []byte, channel chan []byte) {
	select {
	case channel <- event:
	case <-time.After(5 * time.Second):
		// throw away the message, so sad
	}
}

func NewLogWorker(id int, logdir string, bufferCapacity int64) (w *LogWorker) {

	ByteBufferCapacity = bufferCapacity
	return &LogWorker{
		FileRoot:             logdir + "/" + strconv.Itoa(id) + "_",
		buffer:               make([]byte, bufferCapacity),
	}
}

func (w *LogWorker) ListenForLogEvent(channel chan []byte, pending_write_channel chan LogEntry, stats *Stats) {
	for {
		event := <-channel
		length := len(event)

		if Debug > 1 {
			fmt.Println(DateStampAsString(), "Request length:", length)
		}

		if w.CurrMinRequestSize == 0 {
			w.CurrMinRequestSize = int32(length)
		} else if int32(length) < w.CurrMinRequestSize {
			w.CurrMinRequestSize = int32(length)
		}

		if w.CurrMaxRequestSize == 0 {
			w.CurrMaxRequestSize = int32(length)
		} else if int32(length) > w.CurrMaxRequestSize {
			w.CurrMaxRequestSize = int32(length)
		}

		// we run with nginx's client_max_body_size set to 2K which makes this
		// unlikely to happen, but, just in case...
		if length > int(ByteBufferCapacity) {
			fmt.Println(DateStampAsString(), "message received was too large")
			continue
		}

		if Debug == 1 {
			fmt.Println(DateStampAsString(), "Msg length: ", length, ", Position: ", w.position, ", Capacity: ", ByteBufferCapacity)
		}

		if (length + w.position) > int(ByteBufferCapacity) {
			if Debug == 1 {
				fmt.Println(DateStampAsString(), "Dumping buffer to file!")
			}
			w.Save(pending_write_channel)
		}

		copy(w.buffer[w.position:], event)
		w.position += length
		w.UpdateStats(stats)
	}
}

func (w *LogWorker) UpdateRPS(stats *Stats) {

	for {
		mutexRPS.Lock()
		stats.PrevRequestsServed = stats.TotalRequestsServed
		time.Sleep(1 * time.Second)
		stats.RPS = int32(stats.TotalRequestsServed - stats.PrevRequestsServed)
		mutexRPS.Unlock()
		if LoggerConfig.EnableGraphite == 1 {
			fmt.Println(LoggerStats)
			//LoggerStats.GraphiteConn.SimpleSend("logger.metrics.rps", strconv.Itoa(int(stats.RPS)))
		}
		runtime.Gosched()
	}

}

func (w *LogWorker) UpdateStats(stats *Stats) {

	mutexIncr.Lock()
	atomic.AddUint64(&stats.TotalRequestsServed, 1)

	if w.CurrMaxRequestSize > stats.CurrMaxRequestSize {
		stats.CurrMaxRequestSize = w.CurrMaxRequestSize
	}
	if stats.CurrMinRequestSize == 0 || w.CurrMinRequestSize < stats.CurrMinRequestSize {
		stats.CurrMinRequestSize = w.CurrMinRequestSize
	}

	mutexIncr.Unlock()
	runtime.Gosched()
}

func (w *LogWorker) Save(pending_write_channel chan LogEntry) {

	if w.position == 0 {
		return
	}

	// Send the buffer on the channel to be written
	pending_write_channel <- w.buffer[0:w.position]

	// Reset the position of the worker's buffer to 0
	w.position = 0
}


func FileWritter(pending_write_channel chan LogEntry, conf *Config) {

	for {

		lfn := GetLogfileName()
		if lfn != conf.CurrentLogFile {

			//defer currentLogFileHandle.Close()
			mutexCreate.Lock()
			if Debug == 1 {
				fmt.Println(DateStampAsString(), "Could not open file to append data, attempting to create file..")
			}
			fh, err := os.Create(strings.TrimRight(conf.LogDir, "/") + "/" + GetLogfileName())
			if err != nil {
				fmt.Println(DateStampAsString(), "ERROR: Worker could not open new log file!")
				panic(err)
			}

			conf.CurrentLogFileHandle = fh
			defer conf.CurrentLogFileHandle.Close()
			conf.CurrentLogFile = lfn
			mutexCreate.Unlock()
			runtime.Gosched()

		}

		data := <-pending_write_channel

		mutexWrite.Lock()
		nb,err := conf.CurrentLogFileHandle.Write([]byte(data))
		
		if conf.Debug == 1 {
		   if err != nil {
		      fmt.Println(DateStampAsString(), "Write error:", err)
		   }
           fmt.Println(DateStampAsString(), "Wrote ", nb , " bytes to ", conf.CurrentLogFile)
        }

		if conf.ForceFsync == 1 && err == nil {
		   sync_err := conf.CurrentLogFileHandle.Sync()
		   if sync_err != nil && conf.Debug == 1 {
		      fmt.Println(DateStampAsString(), "Sync ERROR:", sync_err)
		   }
		} 

		mutexWrite.Unlock()
		runtime.Gosched()
	}
}