// Library code for the server-server.
package erisdbss

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/eris-ltd/erisdb/server"
	"github.com/gin-gonic/gin"
	"github.com/tendermint/tendermint/binary"
	. "github.com/tendermint/tendermint/common"
	"github.com/tendermint/tendermint/state"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	REAPER_TIMEOUT   = 5 * time.Second
	REAPER_THRESHOLD = 10 * time.Second
	CLOSE_TIMEOUT    = 1 * time.Second
	PORT_BASE        = 29000
	EXECUTABLE_NAME  = "erisdb"
)

const TendermintConfigDefault = `# This is a TOML config file.
# For more information, see https://github.com/toml-lang/toml

moniker = "anothertester"
seeds = ""
fast_sync = false
db_backend = "leveldb"
log_level = "debug"
node_laddr = ""
`

// User data accepts a private validator and genesis json object.
// * PrivValidator is the private validator json data.
// * Genesis is the genesis json data.
// * MaxDuration is the maximum duration of the process (in seconds).
//   If this is 0, it will be set to REAPER_THRESHOLD
// TODO more stuff, like tendermint and server config files. Will probably
// wait with this until the eris/EPM integration.
type RequestData struct {
	PrivValidator *state.PrivValidator `json:"priv_validator"`
	Genesis       *state.GenesisDoc    `json:"genesis"`
	MaxDuration   uint                 `json:"max_duration"`
}

// The response is the URL to the newly generated server.
// TODO return some "live" data after starting the node, so that
// the requester can validate that everything is fine. Maybe
// some data directly from the state manager. Genesis hash?
type ResponseData struct {
	URL string `json:"URL"`
}

// Serves requests to fire up erisdb executables. POSTing to the server
// endpoint (/server by default) with RequestData in the body will create
// a fresh working directory with files based on that indata, fire up a
// new 'erisdb' executable and point it to that dir. The purpose is mostly
// to make testing easier, since setting up a node is as easy as making a
// http request.
// TODO link up with eris/EPM instead, to spawn new nodes in containers.
type ServerServer struct {
	running       bool
	serverManager *ServerManager
}

// Create a new ServerServer with the given base directory.
func NewServerServer(baseDir string) *ServerServer {
	os.RemoveAll(baseDir)
	EnsureDir(baseDir)
	return &ServerServer{serverManager: NewServerManager(100, baseDir)}
}

// Start the server.
func (this *ServerServer) Start(config *server.ServerConfig, router *gin.Engine) {
	router.POST("/server", this.handleFunc)
	this.running = true
}

// Is the server currently running.
func (this *ServerServer) Running() bool {
	return this.running
}

// Shut the server down. Will close all websocket sessions.
func (this *ServerServer) ShutDown() {
	this.running = false
}

// Handle incoming requests.
func (this *ServerServer) handleFunc(c *gin.Context) {
	fmt.Println("INCOMING MESSAGE")
	r := c.Request
	var buf bytes.Buffer
	n, errR := buf.ReadFrom(r.Body)
	if errR != nil || n == 0 {
		http.Error(c.Writer, "Bad request.", 400)
		return
	}
	bts := buf.Bytes()
	var errDC error
	reqData := &RequestData{}
	binary.ReadJSON(reqData, bts, &errDC)
	if errDC != nil {
		http.Error(c.Writer, "Failed to decode json.", 400)
		return
	}
	fmt.Println("STARTING TO ADD")
	resp, errA := this.serverManager.add(reqData)
	if errA != nil {
		http.Error(c.Writer, "Internal error: "+errA.Error(), 500)
		return
	}
	fmt.Printf("WORK DONE: %v\n", resp)
	w := c.Writer
	enc := json.NewEncoder(w)
	enc.Encode(resp)
	w.WriteHeader(200)

}

// A serve task. This wraps a running 'erisdb' process.
type ServeTask struct {
	sp          *exec.Cmd
	workDir     string
	start       time.Time
	maxDuration time.Duration
	port        uint16
}

// Create a new serve task.
func newServeTask(port uint16, workDir string, maxDuration time.Duration, process *exec.Cmd) *ServeTask {
	return &ServeTask{
		process,
		workDir,
		time.Now(),
		maxDuration,
		port,
	}
}

// Catches events that callers subscribe to and adds them to an array ready to be polled.
type ServerManager struct {
	mtx      *sync.Mutex
	idPool   *server.IdPool
	maxProcs uint
	running  []*ServeTask
	reap     bool
	baseDir  string
}

//
func NewServerManager(maxProcs uint, baseDir string) *ServerManager {
	sm := &ServerManager{
		mtx:      &sync.Mutex{},
		idPool:   server.NewIdPool(maxProcs),
		maxProcs: maxProcs,
		running:  make([]*ServeTask, 0),
		reap:     true,
		baseDir:  baseDir,
	}
	go reap(sm)
	return sm
}

func reap(sm *ServerManager) {
	if !sm.reap {
		return
	}
	time.Sleep(REAPER_TIMEOUT)
	sm.mtx.Lock()
	defer sm.mtx.Unlock()
	// The processes are added in order so just read from bottom of array until
	// a time is below reaper threshold, then break.
	for len(sm.running) > 0 {
		task := sm.running[0]
		if time.Since(task.start) > task.maxDuration {
			fmt.Printf("[SERVER REAPER] Closing down server on port: %d\n", task.port)
			task.sp.Process.Kill()
			sm.running = sm.running[1:]
		} else {
			break
		}
	}
	go reap(sm)
}

// Add a new erisdb process to the list.
func (this *ServerManager) add(data *RequestData) (*ResponseData, error) {
	this.mtx.Lock()
	defer this.mtx.Unlock()
	config := server.DefaultServerConfig()
	// Port is PORT_BASE + a value between 1 and the max number of servers.
	port := uint16(PORT_BASE + this.idPool.GetId())
	config.Port = port

	folderName := fmt.Sprintf("testnode%d", port)
	workDir, errCWD := this.createWorkDir(data, config, folderName)
	if errCWD != nil {
		return nil, errCWD
	}
	
	// TODO ...
	
	// Create a new erisdb process.
	proc := exec.Command(EXECUTABLE_NAME, workDir)
	
	reader, errSP := proc.StdoutPipe()
	if errSP != nil {
		return nil, errSP
	}
	
	scanner := bufio.NewScanner(reader)
	scanner.Split(bufio.ScanLines)
	
	if errStart := proc.Start(); errStart != nil {
		return nil, errStart
	}
	
	for scanner.Scan() {
		text := scanner.Text()
		fmt.Println(text)
		if strings.Index(text, "DONTMINDME55891") == -1 {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("Error reading from process stdout:", err)
	}
	
	
	time.Sleep(2000)
	maxDur := time.Duration(data.MaxDuration) * time.Second
	if maxDur == 0 {
		maxDur = REAPER_THRESHOLD
	}
	st := newServeTask(port, workDir, maxDur, proc)
	this.running = append(this.running, st)

	URL := "http://" + config.Address + ":" + fmt.Sprintf("%d", port) + config.JsonRpcPath

	// TODO add validation data. The node should ideally return some post-deploy state data
	// and send it back with the server URL, so that the validity of the chain can be
	// established client-side before starting the tests.
	return &ResponseData{URL: URL}, nil
}

// Creates a temp folder for the tendermint/erisdb node to run in.
// Folder name is port based, so port=1337 meens folder="testnode1337"
// Old folders are cleared out. before creating them, and the server will
// clean out all of this servers workdir (defaults to ~/.edbservers) when
// starting and when stopping.
func (this *ServerManager) createWorkDir(data *RequestData, config *server.ServerConfig, folderName string) (string, error) {

	workDir := path.Join(this.baseDir, folderName)
	os.RemoveAll(workDir)
	errED := EnsureDir(workDir)
	if errED != nil {
		return "", errED
	}

	cfgName := path.Join(workDir, "config.toml")
	scName := path.Join(workDir, "server_conf.toml")
	pvName := path.Join(workDir, "priv_validator.json")
	genesisName := path.Join(workDir, "genesis.json")

	// Write config.
	WriteFile(cfgName, []byte(TendermintConfigDefault))

	// Write validator.
	errPV := writeJSON(data.PrivValidator, pvName)
	if errPV != nil {
		return "", errPV
	}

	// Write genesis
	errG := writeJSON(data.Genesis, genesisName)
	if errG != nil {
		return "", errG
	}

	// Write server config.
	errWC := server.WriteServerConfig(scName, config)
	if errWC != nil {
		return "", errWC
	}

	return workDir, nil
}

func writeJSON(v interface{}, file string) error {
	var n int64
	var errW error
	fo, errC := os.Create(file)
	if errC != nil {
		return errC
	}
	binary.WriteJSON(v, fo, &n, &errW)
	if errW != nil {
		return errW
	}
	errL := fo.Close()
	if errL != nil {
		return errL
	}
	fmt.Printf("File written to %s.\n", file)
	return nil
}