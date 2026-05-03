package main

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Storage wraps an S3 client that talks to LocalStack (or real S3).
type Storage struct {
	client *s3.Client
	bucket string
}

// S3Object is a minimal projection of an S3 object listing entry.
type S3Object struct {
	Key  string
	Size int64
}

func NewStorage(ctx context.Context, cfg Config) (*Storage, error) {
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(cfg.AWSRegion),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			"test", "test", "",
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.S3Endpoint)
		o.UsePathStyle = true // required for LocalStack
	})

	st := &Storage{client: client, bucket: cfg.S3Bucket}

	// Best-effort bucket create; ignore errors if it already exists
	_, _ = client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(cfg.S3Bucket),
	})

	return st, nil
}

// Put writes bytes to a key with the given content-type.
func (s *Storage) Put(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	return nil
}

// Get reads bytes for a key, returning the content-type alongside.
func (s *Storage) Get(ctx context.Context, key string) ([]byte, string, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, "", fmt.Errorf("s3 get %s: %w", key, err)
	}
	defer out.Body.Close()

	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, "", err
	}
	ct := ""
	if out.ContentType != nil {
		ct = *out.ContentType
	}
	return data, ct, nil
}

// List returns every object whose key starts with prefix.
func (s *Storage) List(ctx context.Context, prefix string) ([]S3Object, error) {
	out := make([]S3Object, 0)
	var token *string
	for {
		resp, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("s3 list %s: %w", prefix, err)
		}
		for _, o := range resp.Contents {
			obj := S3Object{}
			if o.Key != nil {
				obj.Key = *o.Key
			}
			if o.Size != nil {
				obj.Size = *o.Size
			}
			out = append(out, obj)
		}
		if resp.IsTruncated == nil || !*resp.IsTruncated {
			break
		}
		token = resp.NextContinuationToken
	}
	return out, nil
}
