package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"golang.org/x/sync/errgroup"
)

type Object struct {
	Key      string
	Modified time.Time
	Size     int64
}

type Store interface {
	Upload(context.Context, string, string) error
	Verify(context.Context, string, int64, string) error
	Publish(context.Context, string, Manifest) error
	ReadManifest(context.Context, string) (Manifest, error)
	Head(context.Context, string) (int64, error)
	Absent(context.Context, string) (bool, error)
	List(context.Context, string) ([]Object, error)
	Delete(context.Context, string) error
	Versioning(context.Context) (string, error)
}

type S3Store struct {
	Client, AbortClient *s3.Client
	Config              S3Config
}

type deadlineConn struct {
	net.Conn
	readTimeout time.Duration
}

func (c deadlineConn) Read(b []byte) (int, error) {
	if err := c.SetReadDeadline(time.Now().Add(c.readTimeout)); err != nil {
		return 0, err
	}

	return c.Conn.Read(b)
}

func (c deadlineConn) Write(b []byte) (int, error) {
	if err := c.SetWriteDeadline(time.Now().Add(c.readTimeout)); err != nil {
		return 0, err
	}

	return c.Conn.Write(b)
}

func NewS3(c S3Config, creds Credentials) *S3Store {
	build := func(connect, read, attempts int) *s3.Client {
		dialer := &net.Dialer{Timeout: time.Duration(connect) * time.Second, KeepAlive: 30 * time.Second}
		transport := &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   time.Duration(connect) * time.Second,
			ResponseHeaderTimeout: time.Duration(read) * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   c.MaxConcurrency + 2,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := dialer.DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}

				return deadlineConn{conn, time.Duration(read) * time.Second}, nil
			},
		}

		return s3.NewFromConfig(
			aws.Config{
				Region: c.Region,
				Credentials: aws.NewCredentialsCache(
					credentials.NewStaticCredentialsProvider(creds.AccessKey, creds.SecretKey, creds.Token),
				),
				HTTPClient: &http.Client{Transport: transport},
				Retryer: func() aws.Retryer {
					return retry.NewStandard(func(o *retry.StandardOptions) { o.MaxAttempts = attempts })
				},
				RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
				ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
			},
			func(o *s3.Options) {
				o.BaseEndpoint = aws.String(c.Endpoint)
				o.UsePathStyle = c.AddressingStyle == "path"
			},
		)
	}

	return &S3Store{build(c.ConnectTimeoutSeconds, c.ReadTimeoutSeconds, c.TotalAttempts), build(2, 3, 1), c}
}

func (s *S3Store) Upload(ctx context.Context, path, key string) (err error) {
	f, err := os.Open(path)
	if err != nil {
		return err
	}

	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}

	c := s.Config
	if st.Size() < c.MultipartThresholdMiB<<20 {
		_, err = s.Client.PutObject(
			ctx,
			&s3.PutObjectInput{
				Bucket:        aws.String(c.Bucket),
				Key:           aws.String(key),
				Body:          f,
				ContentLength: aws.Int64(st.Size()),
				ACL:           types.ObjectCannedACLPrivate,
			},
		)
		return err
	}

	created, err := s.Client.CreateMultipartUpload(
		ctx,
		&s3.CreateMultipartUploadInput{
			Bucket: aws.String(c.Bucket),
			Key:    aws.String(key),
			ACL:    types.ObjectCannedACLPrivate,
		},
	)
	if err != nil {
		return err
	}

	completed := false
	defer func() {
		if !completed {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, abortErr := s.AbortClient.AbortMultipartUpload(
				cleanup,
				&s3.AbortMultipartUploadInput{
					Bucket:   aws.String(c.Bucket),
					Key:      aws.String(key),
					UploadId: created.UploadId,
				},
			)
			if err == nil {
				err = abortErr
			}
		}
	}()
	chunk := c.MultipartChunkSizeMiB << 20
	count := (st.Size() + chunk - 1) / chunk
	if count > 10000 {
		return errors.New("TOO_MANY_PARTS")
	}

	parts := make([]types.CompletedPart, count)
	// SectionReader reads directly from the file. Workers hold no full-part buffers.
	group, workers := errgroup.WithContext(ctx)
	group.SetLimit(c.MaxConcurrency)
	for part := int64(0); part < count; part++ {
		if workers.Err() != nil {
			break
		}

		offset := part * chunk
		size := min(chunk, st.Size()-offset)
		number := part
		group.Go(func() error {
			result, e := s.Client.UploadPart(
				workers,
				&s3.UploadPartInput{
					Bucket:        aws.String(c.Bucket),
					Key:           aws.String(key),
					UploadId:      created.UploadId,
					PartNumber:    aws.Int32(int32(number + 1)),
					Body:          io.NewSectionReader(f, offset, size),
					ContentLength: aws.Int64(size),
				},
			)
			if e != nil {
				return e
			}

			parts[number] = types.CompletedPart{PartNumber: aws.Int32(int32(number + 1)), ETag: result.ETag}
			return nil
		})
	}

	if err = group.Wait(); err != nil {
		return err
	}

	if err = ctx.Err(); err != nil {
		return err
	}

	_, err = s.Client.CompleteMultipartUpload(
		ctx,
		&s3.CompleteMultipartUploadInput{
			Bucket:          aws.String(c.Bucket),
			Key:             aws.String(key),
			UploadId:        created.UploadId,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
		},
	)
	completed = err == nil
	return err
}

func (s *S3Store) Head(ctx context.Context, key string) (int64, error) {
	o, err := s.Client.HeadObject(
		ctx,
		&s3.HeadObjectInput{Bucket: aws.String(s.Config.Bucket), Key: aws.String(key)},
	)
	if err != nil {
		return 0, err
	}

	return aws.ToInt64(o.ContentLength), nil
}

func (s *S3Store) Absent(ctx context.Context, key string) (bool, error) {
	_, err := s.Head(ctx, key)
	if err == nil {
		return false, nil
	}

	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NotFound", "NoSuchKey", "404":
			return true, nil
		}
	}

	return false, err
}

func (s *S3Store) Verify(ctx context.Context, key string, size int64, sha string) error {
	head, err := s.Head(ctx, key)
	if err != nil {
		return err
	}

	if head != size {
		return errors.New("HEAD_SIZE_MISMATCH")
	}

	for attempt := 0; attempt < s.Config.TotalAttempts; attempt++ {
		if err = ctx.Err(); err != nil {
			return err
		}

		err = s.verifyOnce(ctx, key, size, sha)
		if err == nil {
			return nil
		}
	}

	return err
}

func (s *S3Store) verifyOnce(ctx context.Context, key string, size int64, sha string) error {
	o, err := s.Client.GetObject(
		ctx,
		&s3.GetObjectInput{Bucket: aws.String(s.Config.Bucket), Key: aws.String(key)},
	)
	if err != nil {
		return err
	}

	defer o.Body.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, io.LimitReader(o.Body, size+1), make([]byte, s.Config.ReadChunkMiB<<20))
	if err != nil {
		return err
	}

	if n != size || aws.ToInt64(o.ContentLength) != size || hex.EncodeToString(h.Sum(nil)) != sha {
		return errors.New("GET_HASH_MISMATCH")
	}

	return nil
}

func (s *S3Store) ReadManifest(ctx context.Context, key string) (Manifest, error) {
	var m Manifest
	o, err := s.Client.GetObject(
		ctx,
		&s3.GetObjectInput{Bucket: aws.String(s.Config.Bucket), Key: aws.String(key)},
	)
	if err != nil {
		return m, err
	}

	defer o.Body.Close()
	if aws.ToInt64(o.ContentLength) > 1<<20 {
		return m, errors.New("MANIFEST_TOO_LARGE")
	}

	raw, err := io.ReadAll(io.LimitReader(o.Body, (1<<20)+1))
	if err != nil {
		return m, err
	}

	if int64(len(raw)) != aws.ToInt64(o.ContentLength) || len(raw) > 1<<20 {
		return m, errors.New("MANIFEST_SIZE_MISMATCH")
	}

	err = decode(raw, manifestSchema, &m)
	return m, err
}

func (s *S3Store) Publish(ctx context.Context, key string, m Manifest) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}

	if err = decode(raw, manifestSchema, &Manifest{}); err != nil {
		return err
	}

	_, err = s.Client.PutObject(
		ctx,
		&s3.PutObjectInput{
			Bucket:      aws.String(s.Config.Bucket),
			Key:         aws.String(key),
			Body:        bytes.NewReader(raw),
			ACL:         types.ObjectCannedACLPrivate,
			ContentType: aws.String("application/json"),
		},
	)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	got, readErr := s.ReadManifest(ctx, key)
	if readErr != nil {
		return readErr
	}

	gotRaw, readErr := json.Marshal(got)
	if readErr != nil {
		return readErr
	}

	if !bytes.Equal(raw, gotRaw) {
		return errors.New("MANIFEST_UNCONFIRMED")
	}

	return nil
}

func (s *S3Store) List(ctx context.Context, prefix string) ([]Object, error) {
	objects := []Object{}
	var token *string
	seen := map[string]bool{}
	objectKeys := map[string]bool{}
	for {
		page, err := s.Client.ListObjectsV2(
			ctx,
			&s3.ListObjectsV2Input{
				Bucket:            aws.String(s.Config.Bucket),
				Prefix:            aws.String(prefix),
				ContinuationToken: token,
			},
		)
		if err != nil {
			return nil, err
		}

		for _, o := range page.Contents {
			key := aws.ToString(o.Key)
			if key == "" || !strings.HasPrefix(key, prefix) || o.LastModified == nil ||
				o.LastModified.IsZero() ||
				o.Size == nil ||
				*o.Size < 0 ||
				objectKeys[key] {
				return nil, errors.New("MALFORMED_LISTING")
			}

			objectKeys[key] = true
			objects = append(
				objects,
				Object{aws.ToString(o.Key), aws.ToTime(o.LastModified), aws.ToInt64(o.Size)},
			)
		}

		if !aws.ToBool(page.IsTruncated) {
			return objects, nil
		}

		next := aws.ToString(page.NextContinuationToken)
		if next == "" || seen[next] {
			return nil, errors.New("INCOMPLETE_LISTING")
		}

		seen[next] = true
		token = page.NextContinuationToken
	}
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	_, err := s.Client.DeleteObject(
		ctx,
		&s3.DeleteObjectInput{Bucket: aws.String(s.Config.Bucket), Key: aws.String(key)},
	)
	return err
}

func (s *S3Store) Versioning(ctx context.Context) (string, error) {
	o, err := s.Client.GetBucketVersioning(
		ctx,
		&s3.GetBucketVersioningInput{Bucket: aws.String(s.Config.Bucket)},
	)
	if err != nil {
		return "", err
	}

	return string(o.Status), nil
}
