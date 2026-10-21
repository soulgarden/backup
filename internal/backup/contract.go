package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

func ProviderContract(ctx context.Context, store *S3Store, canary, evidencePath, reportPath string) error {
	bucket := store.Config.Bucket
	validBucket := regexp.MustCompile(`^backup-fixture-[a-z0-9][a-z0-9.-]*[a-z0-9]$`)
	if !validBucket.MatchString(bucket) || !validBucket.MatchString(canary) || len(bucket) > 63 ||
		len(canary) > 63 ||
		bucket == canary {
		return errors.New("INVALID_FIXTURE_SCOPE")
	}

	evidence, err := os.ReadFile(evidencePath)
	if err != nil {
		return err
	}

	var scope struct {
		Bucket  string `json:"bucket"`
		Limited bool   `json:"limited_access"`
		Access  string `json:"access"`
	}

	if json.Unmarshal(evidence, &scope) != nil || scope.Bucket != bucket || !scope.Limited ||
		scope.Access != "Read/Write/Delete" {
		return errors.New("INVALID_SCOPE_EVIDENCE")
	}

	_, err = store.Client.ListObjectsV2(
		ctx,
		&s3.ListObjectsV2Input{Bucket: aws.String(canary), MaxKeys: aws.Int32(1)},
	)
	var api smithy.APIError
	if !errors.As(err, &api) || (api.ErrorCode() != "AccessDenied" && api.ErrorCode() != "403") {
		return errors.New("CANARY_SCOPE_FAILED")
	}

	status, err := store.Versioning(ctx)
	if err != nil || status != "" {
		return errors.New("VERSIONING_UNSUPPORTED")
	}

	id, err := NewRunID(time.Now())
	if err != nil {
		return err
	}

	storeCopy := *store
	store = &storeCopy
	store.Config.Prefix = "contract/" + id + "/"
	prefix := store.Config.Prefix
	cleanup := func() error {
		clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		objects, e := store.List(clean, prefix)
		if e != nil {
			return e
		}

		for _, o := range objects {
			if e = store.Delete(clean, o.Key); e != nil {
				return e
			}
		}

		return nil
	}

	success := false
	defer func() {
		if !success {
			_ = cleanup()
		}
	}()
	dir, err := os.MkdirTemp("", "backup-contract-")
	if err != nil {
		return err
	}

	defer os.RemoveAll(dir)
	for _, item := range []struct {
		name string
		size int64
	}{{"single", 1024}, {"multipart", 70 << 20}} {
		path := filepath.Join(dir, item.name)
		f, e := os.Create(path)
		if e != nil {
			return e
		}

		_, e = io.CopyN(f, rand.Reader, item.size)
		closeErr := f.Close()
		if e != nil {
			return e
		}

		if closeErr != nil {
			return closeErr
		}

		if e = store.Upload(ctx, path, prefix+item.name); e != nil {
			return e
		}

		size, sha, e := HashFile(path)
		if e != nil {
			return e
		}

		if e = store.Verify(ctx, prefix+item.name, size, sha); e != nil {
			return e
		}
	}

	for n := 0; n < 1001; n++ {
		_, err = store.Client.PutObject(
			ctx,
			&s3.PutObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(fmt.Sprintf("%spages/%d", prefix, n)),
				Body:   strings.NewReader("x"),
			},
		)
		if err != nil {
			return err
		}
	}

	objects, err := store.List(ctx, prefix)
	if err != nil || len(objects) != 1003 {
		return errors.New("PAGINATION_CONTRACT_FAILED")
	}

	mpu, err := store.Client.CreateMultipartUpload(
		ctx,
		&s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(prefix + "abort")},
	)
	if err != nil {
		return err
	}

	err = func() (partErr error) {
		defer func() {
			abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, e := store.AbortClient.AbortMultipartUpload(
				abortCtx,
				&s3.AbortMultipartUploadInput{
					Bucket:   aws.String(bucket),
					Key:      aws.String(prefix + "abort"),
					UploadId: mpu.UploadId,
				},
			)
			if partErr == nil {
				partErr = e
			}
		}()
		f, e := os.Open(filepath.Join(dir, "multipart"))
		if e != nil {
			return e
		}

		defer f.Close()
		_, e = store.Client.UploadPart(
			ctx,
			&s3.UploadPartInput{
				Bucket:        aws.String(bucket),
				Key:           aws.String(prefix + "abort"),
				UploadId:      mpu.UploadId,
				PartNumber:    aws.Int32(1),
				ContentLength: aws.Int64(5 << 20),
				Body:          io.NewSectionReader(f, 0, 5<<20),
			},
		)
		return e
	}()
	if err != nil {
		return err
	}

	uploads, err := store.Client.ListMultipartUploads(
		ctx,
		&s3.ListMultipartUploadsInput{Bucket: aws.String(bucket), Prefix: aws.String(prefix)},
	)
	if err != nil || len(uploads.Uploads) != 0 {
		return errors.New("MPU_ABORT_FAILED")
	}

	if err = cleanup(); err != nil {
		return err
	}

	objects, err = store.List(ctx, prefix)
	if err != nil || len(objects) != 0 {
		return errors.New("CLEANUP_FAILED")
	}

	success = true
	hash := sha256.Sum256(evidence)
	modules := map[string]string{"github.com/aws/aws-sdk-go-v2": aws.SDKVersion}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if strings.HasPrefix(dep.Path, "github.com/aws/") {
				modules[dep.Path] = dep.Version
			}
		}
	}

	return writeReport(
		reportPath,
		map[string]any{
			"bucket":                       bucket,
			"endpoint":                     store.Config.Endpoint,
			"go_version":                   runtime.Version(),
			"sdk_modules":                  modules,
			"request_checksum_calculation": store.Config.RequestChecksumCalculation,
			"response_checksum_validation": store.Config.ResponseChecksumValidation,
			"single_put":                   true,
			"multipart":                    true,
			"full_sha256":                  true,
			"pagination":                   1003,
			"get_versioning":               true,
			"mpu_abort":                    true,
			"aborted_part_bytes":           5 << 20,
			"canary_access_denied":         true,
			"fixture_cleanup":              true,
			"scope_evidence_sha256":        hex.EncodeToString(hash[:]),
		},
	)
}

func VerifyLifecycle(ctx context.Context, s *S3Store, path string) error {
	o, err := s.Client.GetBucketLifecycleConfiguration(
		ctx,
		&s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(s.Config.Bucket)},
	)
	if err != nil {
		return err
	}

	ids := []string{}
	for _, r := range o.Rules {
		if r.Status != "Enabled" || r.AbortIncompleteMultipartUpload == nil ||
			aws.ToInt32(r.AbortIncompleteMultipartUpload.DaysAfterInitiation) != 1 {
			continue
		}

		prefix := aws.ToString(r.Prefix)
		if r.Filter != nil {
			if r.Filter.And != nil || r.Filter.Tag != nil || r.Filter.ObjectSizeGreaterThan != nil ||
				r.Filter.ObjectSizeLessThan != nil {
				continue
			}

			prefix = aws.ToString(r.Filter.Prefix)
		}

		if prefix == s.Config.Prefix {
			ids = append(ids, aws.ToString(r.ID))
		}
	}

	if len(ids) == 0 {
		return errors.New("LIFECYCLE_NOT_CONFIRMED")
	}

	return writeReport(
		path,
		map[string]any{
			"bucket":         s.Config.Bucket,
			"prefix":         s.Config.Prefix,
			"abort_mpu_days": 1,
			"rule_ids":       ids,
			"read_back":      true,
		},
	)
}

func writeReport(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, append(data, '\n'), 0644)
}
