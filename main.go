package main

import (
	//web and cron
	"fmt"
	"github.com/robfig/cron/v3"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"
	//Config file
	"os"
	"sigs.k8s.io/yaml"
	//SFTP
	"archive/tar"
	"compress/gzip"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/exp/slices"
	"io"
	"path/filepath"
	//FTP
	"github.com/secsy/goftp"
	//Statpage
	"strconv"
	"sync"
	//mysql
	"bytes"
	"os/exec"
)

type Cronjob struct {
	Name             string    `json:"name"`
	Crontab          string    `json:"crontab"`
	Isplainftp       bool      `json:"isplainftp"`
	Retryfirstfailed bool      `json:"retryfirstfailed"`
	Ftpurl           string    `json:"ftpurl"`
	Ftpport          string    `json:"ftpport"`
	Ftpuser          string    `json:"ftpuser"`
	Ftppass          string    `json:"ftppass"`
	Foldername       string    `json:"foldername"`
	Filename         string    `json:"filename"`
	Downloadfolders  []DownDir `json:"downloadfolders"`
}

type Mysqljob struct {
	Name       string
	Crontab    string
	Foldername string
	Filename   string
	User       string
	Pass       string
	Host       string
	Port       string
	Database   string
}

type DownDir struct {
	Name      string   `json:"name"`
	Whitelist []string `json:"whitelist"`
	Blacklist []string `json:"blacklist"`
}

type Config struct {
	Mainfolder     string     `json:"mainfolder"`
	Listenport     string     `json:"listenport"`
	Loglocation    string     `json:"loglocation"`
	Cleanupenabled bool       `json:"cleanupenabled"`
	Cleanupcron    string     `json:"cleanupcron"`
	Cleanupmaxage  int        `json:"cleanupmaxage"`
	Cronjobs       []Cronjob  `json:"cronjobs"`
	Mysqljobs      []Mysqljob `json:"mysqljobs"`
}

func IsWanted(name string, dd DownDir, isFolder bool) bool {
	whitelistActive := len(dd.Whitelist) > 0
	isInWhite := slices.Contains(dd.Whitelist, name)
	if !isInWhite {
		isInWhite = slices.Contains(dd.Whitelist, "*"+filepath.Ext(name))
	}
	isInBlack := slices.Contains(dd.Blacklist, name)
	if !isInBlack {
		isInBlack = slices.Contains(dd.Blacklist, "*"+filepath.Ext(name))
	}
	if whitelistActive && !isInWhite && !isFolder {
		//fmt.Println("[NO ]",name)
		return false
	}
	if isInBlack {
		//fmt.Println("[NO ]",name)
		return false
	}
	//fmt.Println("[YES]",name)
	return true
}

func UpdateStatus(name string, wasOk bool, timeTaken time.Duration) {
	if wasOk {
		LastStatus.Store(name, "[OK ] "+timeTaken.String())
	} else {
		LastStatus.Store(name, "[ERR] "+timeTaken.String())
	}
}

func AddMysqljob(mj Mysqljob, c *cron.Cron, config Config) {
	logger.Info("Adding mysqljob", "name", mj.Name, "crontab", mj.Crontab)
	_, err := c.AddFunc(mj.Crontab, func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("recovered panic in mysqljob", "recover", r)
			}
		}()
		logger.Info("mysql backup start", "name", mj.Name)
		command := fmt.Sprintf(`mysqldump -u %s -p%s -h %s -P %s --databases %s | gzip > %s/%s/bak_%s_$(date +\%%Y\%%m\%%d\%%H\%%M).sql.gz`, mj.User, mj.Pass, mj.Host, mj.Port, mj.Database, config.Mainfolder, mj.Foldername, mj.Filename)
		ec := exec.Command("bash", "-c", command)
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		ec.Stdout = &stdout
		ec.Stderr = &stderr
		starttime := time.Now()
		err := ec.Run()
		timeTaken := time.Since(starttime)
		outStr := stdout.String()
		errStr := stderr.String()
		if err != nil {
			logger.Error("mysqldump error", "name", mj.Name, "timeTaken", timeTaken.String(), "stdout", outStr, "stderr", errStr, "err", err)
			UpdateStatus(mj.Name, false, timeTaken)
		} else {
			logger.Info("mysqldump success", "name", mj.Name, "timeTaken", timeTaken.String(), "stdout", outStr, "stderr", errStr)
			UpdateStatus(mj.Name, true, timeTaken)
		}
	})
	if err != nil {
		panic(err)
	}
}

func AddCronjob(cj Cronjob, c *cron.Cron, config Config) {
	logger.Info("Adding cronjob", "name", cj.Name, "crontab", cj.Crontab)
	_, err := c.AddFunc(cj.Crontab, func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("recovered panic in cronjob", "recover", r)
			}
		}()
		logger.Info("backup start", "name", cj.Name)
		starttime := time.Now()
		var err error
		if cj.Isplainftp {
			err = DownloadFTP(cj, config)
		} else {
			err = DownloadSFTP(cj, config)
			if err != nil && cj.Retryfirstfailed {
				logger.Error("backup failed, will retry in 5s", "name", cj.Name, "err", err)
				time.Sleep(5 * time.Second)
				err = DownloadSFTP(cj, config)
			}
		}
		donetime := time.Now()
		taken := donetime.Sub(starttime)
		if err != nil {
			logger.Error("backup failed", "name", cj.Name, "err", err, "timeTaken", taken.String())
			UpdateStatus(cj.Name, false, taken)
		} else {
			logger.Info("backup succeeded", "name", cj.Name, "err", "", "timeTaken", taken.String())
			UpdateStatus(cj.Name, true, taken)
		}
	})
	if err != nil {
		panic(err)
	}
}

func AddCleanupjob(c *cron.Cron, config Config) {
	logger.Info("AddCleanup", "crontab", config.Cleanupcron, "maxagehours", config.Cleanupmaxage)
	_, err := c.AddFunc(config.Cleanupcron, func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("recovered panic in cleanup", "recover", r)
			}
		}()
		if config.Cleanupmaxage <= 0 {
			logger.Error("cleanup error, cleanupmaxage <= 0, safety cancel")
			return
		}
		logger.Info("cleanup start")
		starttime := time.Now()

		err := filepath.Walk(config.Mainfolder, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			if time.Since(info.ModTime()) > time.Duration(config.Cleanupmaxage)*time.Hour {
				err := os.Remove(path)
				if err != nil {
					logger.Error("cleanup error while deleteing file", "name", "cleanup", "file", path, "err", err)
				} else {
					logger.Info("cleanup deleted file", "name", "cleanup", "file", path)
				}
			}
			return nil
		})

		donetime := time.Now()
		taken := donetime.Sub(starttime)
		if err != nil {
			logger.Error("cleanup failed", "name", "cleanup", "err", err, "timeTaken", taken.String())
			UpdateStatus("cleanup", false, taken)
		} else {
			logger.Info("cleanup succeeded", "name", "cleanup", "err", "", "timeTaken", taken.String())
			UpdateStatus("cleanup", true, taken)
		}
	})
	if err != nil {
		panic(err)
	}
}

func DownloadSFTP(cj Cronjob, config Config) error {

	sshconfig := &ssh.ClientConfig{
		User: cj.Ftpuser,
		Auth: []ssh.AuthMethod{
			ssh.Password(cj.Ftppass),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	conn, err := ssh.Dial("tcp", cj.Ftpurl+":"+cj.Ftpport, sshconfig)
	if err != nil {
		return err
	}
	defer conn.Close()

	client, err := sftp.NewClient(conn)
	if err != nil {
		return err
	}
	defer client.Close()

	file, err := os.Create(filepath.Join(config.Mainfolder, cj.Foldername, cj.Filename+time.Now().Format("_20060102_1504")+".tar.gz"))
	if err != nil {
		return err
	}
	defer file.Close()

	gzipWriter := gzip.NewWriter(file)
	defer gzipWriter.Close()

	tarWriter := tar.NewWriter(gzipWriter)
	defer tarWriter.Close()

	for _, fold := range cj.Downloadfolders {
		finfo, err := client.Stat(fold.Name)
		if err != nil {
			fmt.Println("ERROR DURING sftp client.Stat", err)
			continue
		}
		if finfo.IsDir() {
			err = DownloadSFTPFolder(client, fold.Name, tarWriter, fold)
			if err != nil {
				return err
			}
		} else {
			err = DownloadSFTPFile(client, fold.Name, tarWriter, fold)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func DownloadSFTPFolder(client *sftp.Client, remotePath string, tarWriter *tar.Writer, dd DownDir) error {
	remoteFiles, err := client.ReadDir(remotePath)
	if err != nil {
		return err
	}

	for _, remoteFile := range remoteFiles {
		name := remoteFile.Name()
		remoteFilePath := filepath.Join(remotePath, name)
		if remoteFile.IsDir() {
			if !IsWanted(name, dd, true) {
				continue
			}
			err = DownloadSFTPFolder(client, remoteFilePath, tarWriter, dd)
			if err != nil {
				return err
			}
		} else {
			if !IsWanted(name, dd, false) {
				continue
			}
			err = DownloadSFTPFile(client, remoteFilePath, tarWriter, dd)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func DownloadSFTPFile(client *sftp.Client, remoteFilePath string, tarWriter *tar.Writer, dd DownDir) error {
	remoteFile, err := client.Open(remoteFilePath)
	if err != nil {
		return err
	}
	defer remoteFile.Close()

	info, err := remoteFile.Stat()
	if err != nil {
		return err
	}

	header := &tar.Header{
		Name:    strings.TrimLeft(remoteFilePath, "/"),
		Mode:    int64(info.Mode()),
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}

	err = tarWriter.WriteHeader(header)
	if err != nil {
		return err
	}

	_, err = io.Copy(tarWriter, remoteFile)
	if err != nil {
		return err
	}
	return nil
}

func DownloadFTP(cj Cronjob, config Config) error {
	//Log into nothing with io.Discard, other: os.Stderr
	ftpconfig := goftp.Config{
		User:               cj.Ftpuser,
		Password:           cj.Ftppass,
		ConnectionsPerHost: 2,
		Timeout:            30 * time.Second,
		Logger:             io.Discard,
	}

	client, err := goftp.DialConfig(ftpconfig, cj.Ftpurl+":"+cj.Ftpport)
	if err != nil {
		panic(err)
	}
	defer client.Close()

	file, err := os.Create(filepath.Join(config.Mainfolder, cj.Foldername, cj.Filename+time.Now().Format("_20060102_1504")+".tar.gz"))
	if err != nil {
		return err
	}
	defer file.Close()

	gzipWriter := gzip.NewWriter(file)
	defer gzipWriter.Close()

	tarWriter := tar.NewWriter(gzipWriter)
	defer tarWriter.Close()

	for _, fold := range cj.Downloadfolders {
		finfo, err := client.Stat(fold.Name)
		if err != nil {
			return err
		}
		if finfo.IsDir() {
			err = DownloadFTPFolder(client, fold.Name, tarWriter, fold)
			if err != nil {
				return err
			}
		} else {
			err = DownloadFTPFile(client, fold.Name, tarWriter, fold)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func DownloadFTPFolder(client *goftp.Client, remotePath string, tarWriter *tar.Writer, dd DownDir) error {

	remoteFiles, err := client.ReadDir(remotePath)
	if err != nil {
		return err
	}

	for _, remoteFile := range remoteFiles {
		name := remoteFile.Name()
		remoteFilePath := filepath.Join(remotePath, name)
		if remoteFile.IsDir() {
			if !IsWanted(name, dd, true) {
				continue
			}
			err = DownloadFTPFolder(client, remoteFilePath, tarWriter, dd)
			if err != nil {
				return err
			}
		} else {
			if !IsWanted(name, dd, false) {
				continue
			}
			err = DownloadFTPFile(client, remoteFilePath, tarWriter, dd)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func DownloadFTPFile(client *goftp.Client, remoteFilePath string, tarWriter *tar.Writer, dd DownDir) error {
	info, err := client.Stat(remoteFilePath)
	if err != nil {
		return err
	}

	header := &tar.Header{
		Name:    strings.TrimLeft(remoteFilePath, "/"),
		Mode:    int64(info.Mode()),
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}
	err = tarWriter.WriteHeader(header)
	if err != nil {
		return err
	}

	err = client.Retrieve(remoteFilePath, tarWriter)
	if err != nil {
		return err
	}
	return nil
}

func GetFolderSize(folder string) (string, string) {
	var totalSize int64
	var fileCount int
	err := filepath.Walk(folder, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() {
			totalSize += info.Size()
			fileCount++
		}

		return nil
	})
	if err != nil {
		return "ERR", "ERR"
	}
	return strconv.Itoa(fileCount), strconv.FormatFloat(float64(totalSize)/1024/1024, 'f', 2, 64) + " MB"
}

var logger *slog.Logger

var LastStatus sync.Map

func main() {
	VERSION := "3.0"
	starttime := time.Now()
	//read config
	var config Config
	configfile, err := os.ReadFile("./config.yaml")
	if err != nil {
		panic(err)
	}
	err = yaml.Unmarshal(configfile, &config)
	if err != nil {
		panic(err)
	}

	err = os.MkdirAll(config.Mainfolder, 0755)
	if err != nil {
		panic(err)
	}

	//Logger
	f, err := os.OpenFile(config.Loglocation, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	defer f.Close()
	if err != nil {
		fmt.Println("ERROR opening log file")
		panic(err)
	}
	logger = slog.New(slog.NewJSONHandler(f, nil))

	logger.Info("Miyuki is starting up")

	//CronJobs
	cr := cron.New()
	if config.Cleanupenabled {
		logger.Info("Cleanup job enabled")
		AddCleanupjob(cr, config)
		LastStatus.Store("cleanup", "[NEY]")
	} else {
		logger.Info("Cleanup job disabled")
	}

	for _, cj := range config.Cronjobs {
		AddCronjob(cj, cr, config)
		err = os.MkdirAll(filepath.Join(config.Mainfolder, cj.Foldername), 0755)
		if err != nil {
			panic(err)
		}
		LastStatus.Store(cj.Name, "[NEY]")
	}
	for _, mj := range config.Mysqljobs {
		AddMysqljob(mj, cr, config)
		err = os.MkdirAll(filepath.Join(config.Mainfolder, mj.Foldername), 0755)
		if err != nil {
			panic(err)
		}
		LastStatus.Store(mj.Name, "[NEY]")
	}
	cr.Start()

	//Webserver
	tmpl := template.Must(template.New("stats").Parse(webTemplate))

	// Routes
	http.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		logger.Info("webrequest", "url", r.URL.String(), "method", r.Method)
		fileCount, totalSize := GetFolderSize(config.Mainfolder)
		jobmap := make(map[string]string)
		LastStatus.Range(func(key, value interface{}) bool {
			jobmap[key.(string)] = value.(string)
			return true
		})
		err := tmpl.ExecuteTemplate(w, "stats", map[string]interface{}{
			"Title":     "miyuki",
			"Version":   VERSION,
			"JobCount":  strconv.Itoa(len(config.Cronjobs)),
			"Jobs":      jobmap,
			"Totalsize": totalSize,
			"Filecount": fileCount,
		})
		if err != nil {
			logger.Error("ERROR executing template", "err", err)
		}
	})

	donetime := time.Now()
	logger.Info("miyuki started", "time", donetime.Sub(starttime), "port", config.Listenport)
	fmt.Println("Miyuki started on port ", config.Listenport)
	logger.Error("http.ListenAndServe error", "err", http.ListenAndServe(":"+config.Listenport, nil))
}

var webTemplate = `<!DOCTYPE HTML>
<html>
    <head>
    <title>{{.Title}}</title>
    <link rel="icon" type="image/png" href="https://luctus.at/logos/miyuki.png">
    <meta http-equiv="content-type" content="text/html; charset=UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <style>
    body{
        margin:0;
        font-family: Verdana, Arial;
        background-color: GhostWhite;
    }
    #main{
        margin: 0 auto;
        width:1000px;
        max-width: 95%;
        margin-top: 70px;
        padding: 5px;
    }
    td { font-family: consolas,monospace; }
    </style>
    </head>
    <body>
        <div id="main">
            <img src="https://luctus.at/logos/miyuki.png" style="max-height:96px;border: 1px solid black;">
            <h1>{{.Title}}</h1>
            <p>Version: {{.Version}}<br><br>
            Total size: {{.Totalsize}}<br>
            Filecount: {{.Filecount}}<br><br>
            Job count: {{.JobCount}}<br>
            <table><tr><th>name</th><th>stat</th></tr>
                {{range $k, $v := .Jobs}}
                    <tr><td>{{$k}}</td><td>{{$v}}</td></tr>
                {{end}}
            </table>
            </p>
        </div>
    </body>
</html>
`
