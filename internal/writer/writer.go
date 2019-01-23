package writer

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/golang/snappy"
	"github.com/percona/percona-backup-mongodb/internal/awsutils"
	"github.com/percona/percona-backup-mongodb/internal/storage"
	pb "github.com/percona/percona-backup-mongodb/proto/messages"
	"github.com/pierrec/lz4"
	"github.com/pkg/errors"
)

type BackupWriter struct {
	writers   []io.WriteCloser
	wg        *sync.WaitGroup
	lastError error
}

type flusher interface {
	Flush() error
}

func (bw *BackupWriter) Close() error {
	var err error
	for i := len(bw.writers) - 1; i >= 0; i-- {
		if _, ok := bw.writers[i].(flusher); ok {
			if err = bw.writers[i].(flusher).Flush(); err != nil {
				return fmt.Errorf("error fluashing writer %d: %s", i, err)
			}
		}
		if err = bw.writers[i].Close(); err != nil {
			return fmt.Errorf("error closing writer %d: %s", i, err)
		}
	}
	bw.wg.Wait()
	return nil
}

func (bw *BackupWriter) Write(p []byte) (int, error) {
	return bw.writers[len(bw.writers)-1].Write(p)
}

func NewBackupWriter(name string, stg storage.Storage, compressionType pb.CompressionType,
	cypher pb.Cypher) (*BackupWriter, error) {
	bw := &BackupWriter{
		writers: []io.WriteCloser{},
		wg:      &sync.WaitGroup{},
	}

	switch strings.ToLower(stg.Type) {
	case "filesystem":
		filepath := path.Join(stg.Filesystem.Path, name)
		fw, err := os.Create(filepath)
		if err != nil {
			return nil, errors.Wrapf(err, "Cannot create destination file: %s", filepath)
		}
		bw.writers = append(bw.writers, fw)
	case "s3":
		awsSession, err := awsutils.GetAWSSessionFromStorage(stg.S3)
		if err != nil {
			return nil, errors.Wrap(err, "cannot get an AWS session")
		}
		// s3.Uploader runs synchronously and receives an io.Reader but here, we are implementing
		// writers so, we need to create an io.Pipe and run uploader.Upload in a go-routine
		pr, pw := io.Pipe()
		go func() {
			uploader := s3manager.NewUploader(awsSession)
			bw.wg.Add(1)
			_, bw.lastError = uploader.Upload(&s3manager.UploadInput{
				Bucket: aws.String(stg.S3.Bucket),
				Key:    aws.String(name),
				Body:   pr,
			})
			// make Close() to wait until the upload has finished
			bw.wg.Done()
		}()
		bw.writers = append(bw.writers, pw)
	default:
		return nil, fmt.Errorf("Don't know how to handle %q storage type", stg.Type)
	}

	switch compressionType {
	case pb.CompressionType_COMPRESSION_TYPE_GZIP:
		gzw := gzip.NewWriter(bw.writers[len(bw.writers)-1])
		bw.writers = append(bw.writers, gzw)
	case pb.CompressionType_COMPRESSION_TYPE_LZ4:
		lz4w := lz4.NewWriter(bw.writers[len(bw.writers)-1])
		bw.writers = append(bw.writers, lz4w)
	case pb.CompressionType_COMPRESSION_TYPE_SNAPPY:
		snappyw := snappy.NewWriter(bw.writers[len(bw.writers)-1])
		bw.writers = append(bw.writers, snappyw)
	}

	switch cypher {
	case pb.Cypher_CYPHER_NO_CYPHER:
		//TODO: Add cyphers
	}

	if len(bw.writers) == 0 {
		return nil, fmt.Errorf("there are no backup writers")
	}
	return bw, nil
}
