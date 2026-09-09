// Package s3 implements the objectstore.Store seam over AWS S3 (R6-Q7) using
// aws-sdk-go-v2. Credentials come from the default SDK chain: IMDSv2 instance
// role in the container (shares the host netns, no keys in the image), env
// credentials for local development. Uploads are plain PutObject calls — no
// multipart manager (R12-Q9: bundles are small).
package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/ChronicCmposer/gitd/internal/objectstore"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Store implements objectstore.Store on one bucket. Keys are the full logical
// keys (e.g. "repos/<repo>/<timestamp>.bundle"); the storage.prefix config
// value is part of those keys, composed by the mirror package.
type Store struct {
	client *s3.Client
	bucket string
}

// New builds an s3-backed store. Region and the default credential chain come
// from the SDK config (R6-Q7: IMDSv2 instance role, env creds for local dev).
func New(ctx context.Context, bucket, region string) (*Store, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return &Store{client: s3.NewFromConfig(cfg), bucket: bucket}, nil
}

// Put implements objectstore.Store (R12-Q9: plain PutObject).
func (s *Store) Put(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("s3 put %s/%s: %w", s.bucket, key, err)
	}
	return nil
}

// Get implements objectstore.Store.
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 get %s/%s: %w", s.bucket, key, err)
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("s3 get %s/%s: read body: %w", s.bucket, key, err)
	}
	return data, nil
}

// List implements objectstore.Store, paginating ListObjectsV2.
func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("s3 list %s/%s: %w", s.bucket, prefix, err)
		}
		for _, obj := range out.Contents {
			keys = append(keys, aws.ToString(obj.Key))
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}
	sort.Strings(keys)
	return keys, nil
}

// Delete implements objectstore.Store. S3 deletes are idempotent, so a
// missing key is not an error.
func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3 delete %s/%s: %w", s.bucket, key, err)
	}
	return nil
}

// compile-time check that Store satisfies the consumer-side seam.
var _ objectstore.Store = (*Store)(nil)
