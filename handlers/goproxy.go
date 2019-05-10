package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/air-gases/cacheman"
	"github.com/aofei/air"
	"github.com/cespare/xxhash/v2"
	"github.com/goproxy/goproxy.cn/cfg"
	"github.com/qiniu/api.v7/auth/qbox"
	"github.com/qiniu/api.v7/storage"
	"github.com/rs/zerolog/log"
)

var (
	goBinWorkerChan chan struct{}

	localCacheMutex     sync.Mutex
	localCacheWaitGroup sync.WaitGroup

	qiniuMac                  *qbox.Mac
	qiniuStorageConfig        *storage.Config
	qiniuStorageBucketManager *storage.BucketManager

	invalidModOutputKeywords = []string{
		"could not read username",
		"invalid",
		"malformed",
		"no matching",
		"not found",
		"unknown",
		"unrecognized",
	}

	errModuleNotFound = errors.New("module not found")
)

func init() {
	qiniuMac = qbox.NewMac(
		cfg.Goproxy.QiniuAccessKey,
		cfg.Goproxy.QiniuSecretKey,
	)

	qiniuStorageRegion, err := storage.GetRegion(
		cfg.Goproxy.QiniuAccessKey,
		cfg.Goproxy.QiniuStorageBucket,
	)
	if err != nil {
		log.Fatal().Err(err).
			Str("app_name", a.AppName).
			Msg("failed to get qiniu storage region client")
	}

	qiniuStorageConfig = &storage.Config{
		Region: qiniuStorageRegion,
	}

	qiniuStorageBucketManager = storage.NewBucketManager(
		qiniuMac,
		qiniuStorageConfig,
	)

	goproxyRoot := filepath.Join(os.TempDir(), "goproxy")

	if err := os.Setenv(
		"GOCACHE",
		filepath.Join(goproxyRoot, "gocache"),
	); err != nil {
		log.Fatal().Err(err).
			Str("app_name", a.AppName).
			Msg("failed to set $GOCACHE")
	}

	if err := os.Setenv(
		"GOPATH",
		filepath.Join(goproxyRoot, "gopath"),
	); err != nil {
		log.Fatal().Err(err).
			Str("app_name", a.AppName).
			Msg("failed to set $GOPATH")
	}

	goBinWorkerChan = make(chan struct{}, cfg.Goproxy.MaxGoBinWorkers)

	go func() {
		for {
			startTime := time.Now()

			var totalSize int64
			filepath.Walk(goproxyRoot, func(
				_ string,
				fi os.FileInfo,
				err error,
			) error {
				if fi != nil && !fi.IsDir() {
					totalSize += fi.Size()
				}

				return err
			})

			if totalSize > int64(cfg.Goproxy.MaxLocalCacheBytes) {
				localCacheMutex.Lock()

				localCacheWaitGroup.Wait()
				os.RemoveAll(goproxyRoot)

				localCacheMutex.Unlock()
			}

			if d := time.Now().Sub(startTime); d < 10*time.Minute {
				time.Sleep(10*time.Minute - d)
			}
		}
	}()

	a.BATCH(
		[]string{http.MethodGet, http.MethodHead},
		"/*",
		goproxyHandler,
		cacheman.Gas(cacheman.GasConfig{
			MustRevalidate: true,
			NoCache:        true,
			NoStore:        true,
			MaxAge:         -1,
			SMaxAge:        -1,
		}),
	)
}

// goproxyHandler handles requests to perform a Go module proxy action.
func goproxyHandler(req *air.Request, res *air.Response) error {
	localCacheMutex.Lock()
	localCacheMutex.Unlock()

	localCacheWaitGroup.Add(1)
	defer localCacheWaitGroup.Done()

	filename := req.Param("*").Value().String()
	filenameParts := strings.Split(filename, "/@")
	if len(filenameParts) != 2 {
		return a.NotFoundHandler(req, res)
	}

	encodedModulePath := filenameParts[0]
	modulePath := decodeModulePathOrVersion(encodedModulePath)
	if modulePath == "" {
		return a.NotFoundHandler(req, res)
	}

	switch filenameParts[1] {
	case "v/list", "latest":
		mlr, err := modList(req.Context, modulePath)
		if err != nil {
			if err == errModuleNotFound {
				return a.NotFoundHandler(req, res)
			}

			return err
		}

		switch filenameParts[1] {
		case "v/list":
			return res.WriteString(strings.Join(mlr.Versions, "\n"))
		case "latest":
			mlr.Versions = nil // No need
			return res.WriteJSON(mlr)
		}
	}

	fileInfo, err := qiniuStorageBucketManager.Stat(
		cfg.Goproxy.QiniuStorageBucket,
		filename,
	)
	if isFileNotExist(err) {
		filenameBase := path.Base(filenameParts[1])
		filenameExt := path.Ext(filenameBase)
		switch filenameExt {
		case ".info", ".mod", ".zip":
		default:
			return a.NotFoundHandler(req, res)
		}

		encodedModuleVersion := strings.TrimSuffix(
			filenameBase,
			filenameExt,
		)
		moduleVersion := decodeModulePathOrVersion(encodedModuleVersion)
		if moduleVersion == "" {
			return a.NotFoundHandler(req, res)
		}

		_, err := modDownload(req.Context, modulePath, moduleVersion)
		if err != nil {
			if err == errModuleNotFound {
				return a.NotFoundHandler(req, res)
			}

			return err
		}

		if fileInfo, err = qiniuStorageBucketManager.Stat(
			cfg.Goproxy.QiniuStorageBucket,
			filename,
		); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	fileReq, err := http.NewRequest(
		http.MethodGet,
		storage.MakePrivateURL(
			qiniuMac,
			cfg.Goproxy.QiniuStorageBucketAccessEndpoint,
			filename,
			time.Now().Add(time.Hour).Unix(),
		),
		nil,
	)
	if err != nil {
		return err
	}

	fileReq = fileReq.WithContext(req.Context)

	fileRes, err := http.DefaultClient.Do(fileReq)
	if err != nil {
		return err
	}
	defer fileRes.Body.Close()

	res.Header.Set("Content-Type", fileInfo.MimeType)
	res.Header.Set("Content-Length", strconv.FormatInt(fileInfo.Fsize, 10))

	eTag := make([]byte, 8)
	binary.BigEndian.PutUint64(eTag, xxhash.Sum64String(fileInfo.Hash))
	res.Header.Set("ETag", fmt.Sprintf(
		"%q",
		base64.StdEncoding.EncodeToString(eTag),
	))

	res.Header.Set(
		"Last-Modified",
		storage.ParsePutTime(fileInfo.PutTime).Format(http.TimeFormat),
	)

	if path.Base(filename) == path.Base(req.Path) {
		res.Header.Set("Cache-Control", "max-age=31536000")
	}

	_, err = io.Copy(res.Body, fileRes.Body)

	return err
}

// decodeModulePathOrVersion decodes the epov as the module path or version.
func decodeModulePathOrVersion(epov string) string {
	var (
		dpovb strings.Builder
		bang  bool
	)

	dpovb.Grow(len(epov))
	for _, r := range epov {
		if r >= 'A' && r <= 'Z' {
			return ""
		}

		if r == '!' {
			bang = true
			continue
		}

		if bang {
			bang = false
			if r >= 'a' && r <= 'z' {
				r -= 'a' - 'A' // To upper
			} else {
				dpovb.WriteByte('!')
			}
		}

		dpovb.WriteRune(r)
	}

	return dpovb.String()
}

// encodeModulePathOrVersion encodes the dpov as the module path or version.
func encodeModulePathOrVersion(dpov string) string {
	epovb := strings.Builder{}
	epovb.Grow(len(dpov))
	for _, r := range dpov {
		if r >= 'A' && r <= 'Z' {
			epovb.WriteByte('!')
			epovb.WriteRune(r + 'a' - 'A') // To lower
		} else {
			epovb.WriteRune(r)
		}
	}

	return epovb.String()
}

// modListResult is the result of
// `go list -json -m -versions <MODULE_PATH>@latest`.
type modListResult struct {
	Versions []string `json:"Versions,omitempty"`
	Version  string   `json:"Version"`
	Time     string   `json:"Time"`
}

// modList executes `go list -json -m -versions modulePath@latest`.
func modList(ctx context.Context, modulePath string) (*modListResult, error) {
	goBinWorkerChan <- struct{}{}
	defer func() {
		<-goBinWorkerChan
	}()

	cmd := exec.CommandContext(
		ctx,
		cfg.Goproxy.GoBinName,
		"list",
		"-json",
		"-m",
		"-versions",
		modulePath+"@latest",
	)
	stdout := bytes.Buffer{}
	stderr := bytes.Buffer{}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		output := fmt.Sprint(stdout.String(), stderr.String())
		if invalidModOutput(output) {
			return nil, errModuleNotFound
		}

		return nil, fmt.Errorf("modList: %v: %s", err, output)
	}

	mlr := &modListResult{}
	if err := json.Unmarshal(stdout.Bytes(), mlr); err != nil {
		return nil, err
	}

	return mlr, nil
}

// modDownloadResult is the result of
// `go mod download -json <MODULE_PATH>@<MODULE_VERSION>`.
type modDownloadResult struct {
	Info  string `json:"Info"`
	GoMod string `json:"GoMod"`
	Zip   string `json:"Zip"`
}

// modDownload executes `go mod download -json modulePath@moduleVersion`.
func modDownload(
	ctx context.Context,
	modulePath string,
	moduleVersion string,
) (*modDownloadResult, error) {
	goBinWorkerChan <- struct{}{}
	defer func() {
		<-goBinWorkerChan
	}()

	cmd := exec.CommandContext(
		ctx,
		cfg.Goproxy.GoBinName,
		"mod",
		"download",
		"-json",
		modulePath+"@"+moduleVersion,
	)
	stdout := bytes.Buffer{}
	stderr := bytes.Buffer{}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		output := fmt.Sprint(stdout.String(), stderr.String())
		if invalidModOutput(output) {
			return nil, errModuleNotFound
		}

		return nil, fmt.Errorf("modDownload: %v: %s", err, output)
	}

	mdr := &modDownloadResult{}
	if err := json.Unmarshal(stdout.Bytes(), mdr); err != nil {
		return nil, err
	}

	filenamePrefix := path.Join(
		encodeModulePathOrVersion(modulePath),
		"@v",
		encodeModulePathOrVersion(moduleVersion),
	)

	infoFilename := fmt.Sprint(filenamePrefix, ".info")
	if err := uploadFile(
		ctx,
		infoFilename,
		mdr.Info,
		"application/json; charset=utf-8",
	); err != nil {
		return nil, err
	}

	modFilename := fmt.Sprint(filenamePrefix, ".mod")
	if err := uploadFile(
		ctx,
		modFilename,
		mdr.GoMod,
		"text/plain; charset=utf-8",
	); err != nil {
		return nil, err
	}

	zipFilename := fmt.Sprint(filenamePrefix, ".zip")
	if err := uploadFile(
		ctx,
		zipFilename,
		mdr.Zip,
		"application/zip",
	); err != nil {
		return nil, err
	}

	return mdr, nil
}

// invalidModOutput reports whether the mo is a invalid mod output.
func invalidModOutput(mo string) bool {
	mo = strings.ToLower(mo)
	for _, k := range invalidModOutputKeywords {
		if strings.Contains(mo, k) {
			return true
		}
	}

	return false
}

// isFileNotExist reports whether the err indicates that some file does not
// exist.
func isFileNotExist(err error) bool {
	return err != nil && err.Error() == "no such file or directory"
}

// uploadFile uploads the localFilename as the contentType to the Qiniu storage
// bucket. The filename is the new name in the Qiniu storage bucket.
func uploadFile(
	ctx context.Context,
	filename string,
	localFilename string,
	contentType string,
) error {
	return storage.NewFormUploader(qiniuStorageConfig).PutFile(
		ctx,
		nil,
		(&storage.PutPolicy{
			Scope: fmt.Sprintf(
				"%s:%s",
				cfg.Goproxy.QiniuStorageBucket,
				filename,
			),
		}).UploadToken(qiniuMac),
		filename,
		localFilename,
		&storage.PutExtra{
			MimeType: contentType,
		},
	)
}
