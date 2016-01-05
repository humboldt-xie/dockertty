package main
import(
	log "github.com/Sirupsen/logrus"
	"net"
	"io"
	"encoding/json"
	"encoding/base64"
	"bytes"
	"crypto/tls"
	"net/http/httputil"
	"strings"
	"fmt"
	"time"
	"bufio"
	//"strconv"
	"net/http"
	"github.com/gorilla/context"
	"github.com/humboldt-xie/dockertty/utils"
	"github.com/humboldt-xie/dockerclient"
	"golang.org/x/net/websocket"
	"sync"
)
type (
Api struct {
	client           *dockerclient.DockerClient
	allowInsecure    bool
	PermitWrite bool
	writeMutex *sync.Mutex
}
argResizeTerminal struct {
	Columns float64
	Rows    float64
}
)
const (
	Input          = '0'
	Ping           = '1'
	ResizeTerminal = '2'
)

const (
	Output         = '0'
	Pong           = '1'
	SetWindowTitle = '2'
	SetPreferences = '3'
	SetReconnect   = '4'
)

func (a*Api) processSend(conn *websocket.Conn,execId string,br *bufio.Reader) {

	for {
		buf := make([]byte, 1024)
		size,err := br.Read(buf)
		if err != nil {
			log.Printf("Command exited for: %s %s", conn.RemoteAddr,err)
			return
		}
		safeMessage := base64.StdEncoding.EncodeToString([]byte(buf[:size]))
		_,err=conn.Write(append([]byte{Output},[]byte(safeMessage)...))
		if err != nil {
			log.Printf("write err:%s",err.Error())
			return
		}
		
	}
}
func (a*Api) processReceive(conn *websocket.Conn,execId string,rwc io.WriteCloser) {
	for {
		var data= make([]byte, 1024)
		size, err := conn.Read(data)
		if err != nil {
			log.Print(err.Error())
			return
		}
		if len(data) == 0 {
			log.Print("An error has occured")
			return
		}
		log.Printf("read:%s\n",data[:size])
		switch data[0] {
		case Input:
			if !a.PermitWrite {
				break
			}
			_,err :=rwc.Write(data[1:size])
			if err != nil {
				return
			}
		case Ping:
			_,err:=conn.Write([]byte{Pong})
			if err != nil {
				log.Print(err.Error())
				return
			}
		case ResizeTerminal:
			var args argResizeTerminal
			err = json.Unmarshal(data[1:size], &args)
			if err != nil {
				log.Print("Malformed remote command %s %s",err,data[:size])
				return
			}
			if err := a.client.ExecResize(execId, int(args.Columns), int(args.Rows)); err != nil {
				log.Errorf("error resizing exec tty:url:%s %s %s",a.client.URL, err,execId)
			}
		default:
			log.Print("Unknown message type")
			return
		}
	}
}


func (a *Api) hijack(addr, method, execId string, conn *websocket.Conn ) error {
	path:="/exec/"+execId+"/start"
	execConfig := &dockerclient.ExecConfig{
		Tty:    true,
		Detach: false,
	}

	buf, err := json.Marshal(execConfig)
	if err != nil {
		return fmt.Errorf("error marshaling exec config: %s", err)
	}

	rdr := bytes.NewReader(buf)

	req, err := http.NewRequest(method, path, rdr)
	if err != nil {
		return fmt.Errorf("error during hijack request: %s", err)
	}

	req.Header.Set("User-Agent", "Docker-Client")
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	req.Host = addr

	var (
		dial          net.Conn
		dialErr       error
		execTLSConfig = a.client.TLSConfig
	)


	if execTLSConfig == nil {
		dial, dialErr = net.Dial("tcp", addr)
	} else {
		if a.allowInsecure {
			execTLSConfig.InsecureSkipVerify = true
		}
		log.Debug("using tls for exec hijack")
		dial, dialErr = tls.Dial("tcp", addr, execTLSConfig)
	}

	if dialErr != nil {
		return dialErr
	}

	// When we set up a TCP connection for hijack, there could be long periods
	// of inactivity (a long running command with no output) that in certain
	// network setups may cause ECONNTIMEOUT, leaving the client in an unknown
	// state. Setting TCP KeepAlive on the socket connection will prohibit
	// ECONNTIMEOUT unless the socket connection truly is broken
	if tcpConn, ok := dial.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}
	if err != nil {
		return err
	}
	clientconn := httputil.NewClientConn(dial, nil)

	// Server hijacks the connection, error 'connection closed' expected
	clientconn.Do(req)

	rwc, br := clientconn.Hijack()
	/*for {
		buf := make([]byte, 1024)
		size,err := br.Read(buf)
		if err != nil {
			log.Printf("Command exited for: %s %s %d %s", conn.RemoteAddr,err,size,buf,rwc)
			return nil
		}
		conn.Write(append([]byte{Output},buf[:size]...));
		
	}*/

	exit := make(chan bool, 2)

	go func(){
		defer func() { exit <- true }()
		a.processReceive(conn,execId,rwc)
	}()

	go func() {
		defer func() { exit <- true }()
		a.processSend(conn,execId,br)
	}()
	<-exit
	clientconn.Close()
	rwc.Close()

	return nil
}
func (a *Api) execContainer(ws *websocket.Conn) {
	//qry := ws.Request().URL.Query()
	//containerId := qry.Get("id")
	containerId := "centos";
	command := "bash" //qry.Get("cmd")
	//token := qry.Get("token")
	cmd := strings.Split(command, ",")

	log.Debugf("starting exec session: container=%s cmd=%s", containerId, command)
	clientUrl := a.client.URL

	execConfig := &dockerclient.ExecConfig{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
		Cmd:          cmd,
		Container:    containerId,
		Detach:       true,
	}

	execId, err := a.client.ExecCreate(execConfig)
	if err != nil {
		log.Errorf("error calling exec: %s", err)
		return
	}
	log.Debug("exec %s",execId)

	if err := a.hijack(clientUrl.Host, "POST", execId, ws); err != nil {
		log.Errorf("error during hijack: %s", err)
		return
	}

}

func main() {
	globalMux := http.NewServeMux()
	dockerUrl:="tcp://127.0.0.1:5554" 
	tlsCaCert:=""
	tlsCert:=""
	tlsKey:=""
	allowInsecure:=true
	log.SetLevel(log.DebugLevel)
	client, err := utils.GetClient(dockerUrl, tlsCaCert, tlsCert, tlsKey, allowInsecure)
	if err != nil {
		log.Fatal(err)
	}
	a:= &Api{client,true,true,&sync.Mutex{}}

	globalMux.Handle("/", http.FileServer(http.Dir("static")))
	globalMux.Handle("/exec", websocket.Handler(a.execContainer))
	globalMux.Handle("/ws", websocket.Handler(a.execContainer))
	s := &http.Server{
		Addr:    "0.0.0.0:8081",
		Handler: context.ClearHandler(globalMux),
	}
	var runErr error
	runErr = s.ListenAndServe()
	if runErr != nil {
		log.Fatal(runErr)
	}

}
