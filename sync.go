package main

import (
	"compress/gzip"
	"crypto/md5"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudfront"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
)

var contentTypeMap = map[string]string{
	"css":  "text/css",
	"html": "text/html",
	"htm":  "text/html",
	"ico":  "image/x-ico",
	"js":   "text/javascript",
	"jpg":  "image/jpeg",
	"gif":  "image/gif",
	"png":  "image/png",
	"xml":  "application/xml",
	"svg":  "image/svg+xml",
	"jpeg": "image/jpeg",
}

var compressBlacklist = map[string]bool{
	"gif":  true,
	"jpg":  true,
	"png":  true,
	"jpeg": true,
	"psd":  true,
	"ai":   true,
}

type FileInfo struct {
	path     string
	key      string
	fileInfo os.FileInfo
}

func Sync(sess *session.Session, bucket string, path string, reUpload bool, concurrentNum int) {
	svc := s3.New(sess)

	s3Keys := map[string]string{}
	updatedKeys := make([]*string, 0, 100)

	err := svc.ListObjectsV2Pages(&s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
	}, func(page *s3.ListObjectsV2Output, lastPage bool) bool {
		for _, s3Object := range page.Contents {
			etag := *s3Object.ETag
			etag = etag[1 : len(etag)-1]
			s3Keys[*s3Object.Key] = etag
		}
		return true
	})

	DisplayAwsErr(err)

	localFilesChan := make(chan *FileInfo, 100)
	doneChan := make(chan *string, 100)
	wg := sync.WaitGroup{}

	uploader := s3manager.NewUploader(sess)

	go GetAllFiles(path, localFilesChan)

	wg.Add(concurrentNum)
	for i := 0; i < concurrentNum; i++ {
		go UploadFileHandler(uploader, localFilesChan, &wg, bucket, &s3Keys, reUpload, doneChan)
	}

	go func() {
		for key := range doneChan {
			updatedKeys = append(updatedKeys, key)
		}
	}()
	wg.Wait()

	InvalidCloudFront(sess, bucket, &updatedKeys)
}

func InvalidCloudFront(sess *session.Session, domain string, paths *[]*string) {
	if len(*paths) == 0 {
		return
	}

	distributionId := ""

	svc := cloudfront.New(sess)

	listDistInput := &cloudfront.ListDistributionsInput{
	// TODO: Marker: Handle truncated result
	}

	resp, err := svc.ListDistributions(listDistInput)

	for _, distribution := range resp.DistributionList.Items {
		for _, cname := range distribution.Aliases.Items {
			if *cname == domain {
				distributionId = *distribution.Id
			}
		}
		if distributionId != "" {
			break
		}
	}

	invalidationInput := &cloudfront.CreateInvalidationInput{
		DistributionId: aws.String(distributionId),
		InvalidationBatch: &cloudfront.InvalidationBatch{
			CallerReference: aws.String(GetCallerReference()),
			Paths: &cloudfront.Paths{
				Quantity: aws.Int64(int64(len(*paths))),
				Items:    *paths,
			},
		},
	}

	fmt.Println("Send invalidate to Dist ID: " + distributionId)
	for _, key := range *paths {
		fmt.Println(*key)
	}
	_, err = svc.CreateInvalidation(invalidationInput)

	DisplayAwsErr(err)
}

func Hashfile(filename string) (string, error) {
	file, err := os.Open(filename)
	if err != nil {
		CheckErr(err)
	}
	defer file.Close()

	hasher := md5.New()
	io.Copy(hasher, file)
	hashVal := fmt.Sprintf("%x", hasher.Sum(nil))
	return hashVal, nil
}

func UploadFileHandler(uploader *s3manager.Uploader, localFilesChan chan *FileInfo, wg *sync.WaitGroup, bucket string, s3Keys *map[string]string, reUpload bool, doneChan chan *string) {
	defer wg.Done()

	for file := range localFilesChan {
		uploadPath := file.path

		contentEncoding := ""
		suffix := strings.ToLower(filepath.Ext(file.path))
		if !compressBlacklist[suffix] && file.fileInfo.Size() > 500 {
			fmt.Println("Compressing: " + file.path)

			compressedFile, err := ioutil.TempFile("", "oursky")
			if err != nil {
				CheckErr(err)
			}
			gzipper, _ := gzip.NewWriterLevel(compressedFile, gzip.BestCompression)
			fileInput, err := os.Open(file.path)
			if err != nil {
				CheckErr(err)
			}
			io.Copy(gzipper, fileInput)
			fileInput.Close()
			gzipper.Close()
			uploadPath = compressedFile.Name()

			contentEncoding = "gzip"
		}

		// Determine MIME type quick
		contentType, ok := contentTypeMap[suffix]
		if !ok {
			f, err := os.Open(file.path)
			CheckErr(err)

			byte512 := make([]byte, 512)
			_, err = f.Read(byte512)
			if err != io.EOF {
				CheckErr(err)
			}

			contentType = http.DetectContentType(byte512)
			fmt.Println("Detected MIME: " + contentType)
		}

		hash, err := Hashfile(uploadPath)
		if err != nil {
			fmt.Println("Hash error: " + file.path)
		}

		etag, ok := (*s3Keys)[file.key]
		if ok && !reUpload && etag == hash {
			continue
		}

		fmt.Println("Uploading: " + uploadPath + " as " + file.key)
		fileIO, err := os.Open(uploadPath)

		upParams := &s3manager.UploadInput{
			Bucket:          aws.String(bucket),
			Key:             aws.String(file.key),
			Body:            fileIO,
			CacheControl:    aws.String("max-age=900"),
			ContentEncoding: aws.String(contentEncoding),
			ContentType:     aws.String(contentType),
			ACL:             aws.String("public-read"),
		}
		_, err = uploader.Upload(upParams)

		DisplayAwsErr(err)

		if err == nil {
			key, _ := url.ParseRequestURI("/" + file.key)
			doneChan <- aws.String(key.String())
		}
	}
}

func GetAllFiles(dirname string, localFilesChan chan *FileInfo) {
	var scan = func(path string, fileInfo os.FileInfo, err error) error {
		if path == "." {
			return nil
		}
		if !fileInfo.IsDir() && filepath.Base(path)[0] != '.' {
			key := strings.TrimPrefix(path, dirname)
			key = strings.TrimPrefix(key, "/")
			localFilesChan <- &FileInfo{path, key, fileInfo}
		}
		if fileInfo.IsDir() && filepath.Base(path)[0] == '.' {
			return filepath.SkipDir
		}
		return nil
	}

	filepath.Walk(dirname, scan)
	close(localFilesChan)
}
