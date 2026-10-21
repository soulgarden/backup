package backup

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"golang.org/x/sync/errgroup"
)

type ContractOptions struct {
	DryRun        bool
	RunnerVersion string
}

const ContractCleanupTimeout = 5 * time.Minute

func ProviderContract(ctx context.Context, store *S3Store, options ContractOptions, reportPath string) (resultErr error) {
	bucket := store.Config.Bucket
	if err := writeReport(reportPath, map[string]any{
		"status":         "preflight",
		"bucket":         bucket,
		"runner_version": options.RunnerVersion,
	}); err != nil {
		return err
	}
	status, err := store.Versioning(ctx)
	if err != nil || status != "" {
		return errors.New("VERSIONING_UNSUPPORTED")
	}

	id, err := NewRunID(time.Now())
	if err != nil {
		return err
	}

	backupPrefix := store.Config.Prefix
	storeCopy := *store
	store = &storeCopy
	store.Config.Prefix = "_backup-contract/" + id + "/"
	// Keep probes small and exercise multiple parts regardless of backup tuning.
	store.Config.MultipartThresholdMiB = 64
	store.Config.MultipartChunkSizeMiB = 16
	prefix := store.Config.Prefix
	// Temporary checks must never enter the configured backup namespace.
	if strings.HasPrefix(prefix, backupPrefix) || strings.HasPrefix(backupPrefix, "_backup-contract/") {
		return errors.New("CONTRACT_PREFIX_OVERLAP")
	}
	ownedKeys := []string{prefix + "single", prefix + "multipart"}
	for n := 0; n < 1001; n++ {
		ownedKeys = append(ownedKeys, fmt.Sprintf("%spages/%d", prefix, n))
	}
	if options.DryRun {
		return writeReport(reportPath, map[string]any{
			"status":               "dry_run",
			"bucket":               bucket,
			"endpoint":             store.Config.Endpoint,
			"test_prefix":          prefix,
			"runner_version":       options.RunnerVersion,
			"planned_object_count": len(ownedKeys),
			"planned_checks":       []string{"single_put", "multipart", "full_sha256", "pagination", "mpu_abort", "cleanup"},
		})
	}

	journal := map[string]any{
		"status":           "running",
		"bucket":           bucket,
		"test_prefix":      prefix,
		"runner_version":   options.RunnerVersion,
		"owned_keys":       ownedKeys,
		"cleanup_complete": false,
		"multipart_keys":   []string{prefix + "multipart", prefix + "abort"},
	}
	// Save the complete recovery scope before the first mutation, including keys
	// whose upload could succeed remotely while its response is lost.
	if err = writeReport(reportPath, journal); err != nil {
		return err
	}
	cleanupAttempted := false
	var cleanupErr error
	cleanup := func() error {
		if cleanupAttempted {
			return cleanupErr
		}
		cleanupAttempted = true
		clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), ContractCleanupTimeout)
		defer cancel()
		group, workers := errgroup.WithContext(clean)
		group.SetLimit(store.Config.MaxConcurrency)
		for _, key := range ownedKeys {
			group.Go(func() error { return store.Delete(workers, key) })
		}
		cleanupErr = group.Wait()
		if cleanupErr != nil {
			return cleanupErr
		}
		remaining, err := store.Client.ListMultipartUploads(clean, &s3.ListMultipartUploadsInput{
			Bucket: aws.String(bucket),
			Prefix: aws.String(prefix),
		})
		if err != nil {
			cleanupErr = err
		} else if aws.ToBool(remaining.IsTruncated) || len(remaining.Uploads) != 0 {
			cleanupErr = errors.New("CONTRACT_MULTIPART_REMAINS")
		}
		return cleanupErr
	}

	success := false
	defer func() {
		if !success {
			_ = cleanup()
			journal["status"] = "failed"
			journal["cleanup_complete"] = cleanupErr == nil
			var operation *smithy.OperationError
			if errors.As(resultErr, &operation) {
				journal["failed_operation"] = operation.OperationName
			}
			var status interface{ HTTPStatusCode() int }
			if errors.As(resultErr, &status) {
				journal["http_status"] = status.HTTPStatusCode()
			}
			var network net.Error
			journal["network_timeout"] = errors.As(resultErr, &network) && network.Timeout()
			_ = writeReport(reportPath, journal)
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

		key := prefix + item.name
		if e = store.Upload(ctx, path, key); e != nil {
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

	group, workers := errgroup.WithContext(ctx)
	group.SetLimit(store.Config.MaxConcurrency)
	for n := 0; n < 1001; n++ {
		key := fmt.Sprintf("%spages/%d", prefix, n)
		group.Go(func() error {
			_, err := store.Client.PutObject(
				workers,
				&s3.PutObjectInput{
					Bucket: aws.String(bucket),
					Key:    aws.String(key),
					Body:   strings.NewReader("x"),
				},
			)
			return err
		})
	}
	if err = group.Wait(); err != nil {
		return err
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
		journal["abort_upload_id"] = aws.ToString(mpu.UploadId)
		if e := writeReport(reportPath, journal); e != nil {
			return e
		}
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
			"status":                       "completed",
			"runner_version":               options.RunnerVersion,
			"bucket":                       bucket,
			"test_prefix":                  prefix,
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
			"cleanup_complete":             true,
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

	file, err := os.CreateTemp(filepath.Dir(path), ".backup-report-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Chmod(0644); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
