package main

import (
	//web and cron
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"
	"strings"
	"time"
	//Logging
	ginzap "github.com/gin-contrib/zap"
	"go.uber.org/zap"
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
	//Web
	"embed"
	"html/template"
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

type DownDir struct {
	Name      string   `json:"name"`
	Whitelist []string `json:"whitelist"`
	Blacklist []string `json:"blacklist"`
}

type Config struct {
	Mainfolder     string    `json:"mainfolder"`
	Listenport     string    `json:"listenport"`
	Loglocation    string    `json:"loglocation"`
	Cleanupenabled bool      `json:"cleanupenabled"`
	Cleanupcron    string    `json:"cleanupcron"`
	Cleanupmaxage  int       `json:"cleanupmaxage"`
	Cronjobs       []Cronjob `json:"cronjobs"`
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

func AddCronjob(cj Cronjob, c *cron.Cron, config Config) {
	logger.Infow("Adding cronjob", "name", cj.Name, "crontab", cj.Crontab)
	_, err := c.AddFunc(cj.Crontab, func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorw("recovered panic in cronjob", "recover", r)
			}
		}()
		logger.Infow("backup start", "name", cj.Name)
		starttime := time.Now()
		var err error
		if cj.Isplainftp {
			err = DownloadFTP(cj, config)
		} else {
			err = DownloadSFTP(cj, config)
			if err != nil && cj.Retryfirstfailed {
				logger.Errorw("backup failed, will retry in 5s", "name", cj.Name, "err", err)
				time.Sleep(5 * time.Second)
				err = DownloadSFTP(cj, config)
			}
		}
		donetime := time.Now()
		taken := donetime.Sub(starttime)
		if err != nil {
			logger.Errorw("backup failed", "name", cj.Name, "err", err, "time", taken)
			UpdateStatus(cj.Name, false, taken)
		} else {
			logger.Infow("backup succeeded", "name", cj.Name, "err", "", "time", taken)
			UpdateStatus(cj.Name, true, taken)
		}
	})
	if err != nil {
		panic(err)
	}
}

func AddCleanupjob(c *cron.Cron, config Config) {
	logger.Infow("AddCleanup", "crontab", config.Cleanupcron, "maxagehours", config.Cleanupmaxage)
	_, err := c.AddFunc(config.Cleanupcron, func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorw("recovered panic in cleanup", "recover", r)
			}
		}()
		if config.Cleanupmaxage <= 0 {
			logger.Errorw("cleanup error, cleanupmaxage <= 0, safety cancel")
			return
		}
		logger.Infow("cleanup start")
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
					logger.Infow("cleanup error while deleteing file", "name", "cleanup", "file", path, "err", err)
				} else {
					logger.Infow("cleanup deleted file", "name", "cleanup", "file", path)
				}
			}
			return nil
		})

		donetime := time.Now()
		taken := donetime.Sub(starttime)
		if err != nil {
			logger.Errorw("cleanup failed", "name", "cleanup", "err", err, "time", taken)
			UpdateStatus("cleanup", false, taken)
		} else {
			logger.Infow("cleanup succeeded", "name", "cleanup", "err", "", "time", taken)
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

// Logging
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		t := time.Now()
		logger.Infow("webrequest",
			"url", c.Request.URL.String(),
			"method", c.Request.Method,
			"ret", c.Writer.Status(),
			"ip", c.ClientIP(),
			"duration", t.Sub(start),
			"rsize", c.Writer.Size(),
		)
	}
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

var logger *zap.SugaredLogger

var LastStatus sync.Map

//go:embed all:templates/*
var templates embed.FS

func main() {
	VERSION := "2.0"
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
	cfg := zap.NewProductionConfig()
	cfg.OutputPaths = []string{
		config.Loglocation,
	}
	flogger, err := cfg.Build()
	if err != nil {
		panic(err)
	}
	logger = flogger.Sugar()
	defer func() {
		err := logger.Sync()
		if err != nil {
			panic(err)
		}
	}()

	logger.Infow("Miyuki is starting up")

	//CronJobs
	cr := cron.New()
	if config.Cleanupenabled {
		logger.Infow("Cleanup job enabled")
		AddCleanupjob(cr, config)
		LastStatus.Store("cleanup", "[NEY]")
	} else {
		logger.Infow("Cleanup job disabled")
	}

	for _, cj := range config.Cronjobs {
		AddCronjob(cj, cr, config)
		err = os.MkdirAll(filepath.Join(config.Mainfolder, cj.Foldername), 0755)
		if err != nil {
			panic(err)
		}
		LastStatus.Store(cj.Name, "[NEY]")

	}
	cr.Start()

	//Webserver
	gin.SetMode(gin.ReleaseMode)
	app := gin.New()
	app.Use(ginzap.RecoveryWithZap(flogger, true))
	app.Use(RequestLogger())
	gin.DisableConsoleColor()
	templ := template.Must(template.ParseFS(templates, "templates/*"))
	app.SetHTMLTemplate(templ)

	// Routes
	app.GET("/", func(c *gin.Context) {
		fileCount, totalSize := GetFolderSize(config.Mainfolder)
		jobmap := make(map[string]string)
		LastStatus.Range(func(key, value interface{}) bool {
			jobmap[key.(string)] = value.(string)
			return true
		})
		c.HTML(200, "stats", gin.H{
			"Title":     "miyuki",
			"Version":   VERSION,
			"JobCount":  strconv.Itoa(len(config.Cronjobs)),
			"Jobs":      jobmap,
			"Totalsize": totalSize,
			"Filecount": fileCount,
		})
	})

	donetime := time.Now()
	logger.Infow("miyuki started", "time", donetime.Sub(starttime), "port", config.Listenport)
	fmt.Println("Miyuki started on port ", config.Listenport)
	fmt.Println(app.Run(":" + config.Listenport))
}
