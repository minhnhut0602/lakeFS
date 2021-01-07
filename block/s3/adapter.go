package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	v4 "github.com/aws/aws-sdk-go/aws/signer/v4"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/treeverse/lakefs/block"
	"github.com/treeverse/lakefs/logging"
)

const (
	BlockstoreType = "s3"

	DefaultStreamingChunkSize    = 2 << 19         // 1MiB by default per chunk
	DefaultStreamingChunkTimeout = time.Second * 1 // if we haven't read DefaultStreamingChunkSize by this duration, write whatever we have as a chunk

	ExpireObjectS3Tag = "lakefs_expire_object"
)

var (
	ErrS3          = errors.New("s3 error")
	ErrMissingETag = fmt.Errorf("%w: missing ETag", ErrS3)
)

func resolveNamespace(obj block.ObjectPointer) (block.QualifiedKey, error) {
	qualifiedKey, err := block.ResolveNamespace(obj.StorageNamespace, obj.Identifier)
	if err != nil {
		return qualifiedKey, err
	}
	if qualifiedKey.StorageType != block.StorageTypeS3 {
		return qualifiedKey, block.ErrInvalidNamespace
	}
	return qualifiedKey, nil
}

func resolveNamespacePrefix(opts block.ListOpts) (block.QualifiedPrefix, error) {
	qualifiedPrefix, err := block.ResolveNamespacePrefix(opts.StorageNamespace, opts.Prefix)
	if err != nil {
		return qualifiedPrefix, err
	}
	if qualifiedPrefix.StorageType != block.StorageTypeS3 {
		return qualifiedPrefix, block.ErrInvalidNamespace
	}
	return qualifiedPrefix, nil
}

type Adapter struct {
	s3                    s3iface.S3API
	httpClient            *http.Client
	ctx                   context.Context
	uploadIDTranslator    block.UploadIDTranslator
	streamingChunkSize    int
	streamingChunkTimeout time.Duration
}

func WithHTTPClient(c *http.Client) func(a *Adapter) {
	return func(a *Adapter) {
		a.httpClient = c
	}
}

func WithStreamingChunkSize(sz int) func(a *Adapter) {
	return func(a *Adapter) {
		a.streamingChunkSize = sz
	}
}

func WithStreamingChunkTimeout(d time.Duration) func(a *Adapter) {
	return func(a *Adapter) {
		a.streamingChunkTimeout = d
	}
}

func WithContext(ctx context.Context) func(a *Adapter) {
	return func(a *Adapter) {
		a.ctx = ctx
	}
}

func WithTranslator(t block.UploadIDTranslator) func(a *Adapter) {
	return func(a *Adapter) {
		a.uploadIDTranslator = t
	}
}

func NewAdapter(s3 s3iface.S3API, opts ...func(a *Adapter)) *Adapter {
	a := &Adapter{
		s3:                    s3,
		httpClient:            http.DefaultClient,
		ctx:                   context.Background(),
		uploadIDTranslator:    &block.NoOpTranslator{},
		streamingChunkSize:    DefaultStreamingChunkSize,
		streamingChunkTimeout: DefaultStreamingChunkTimeout,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

func (a *Adapter) WithContext(ctx context.Context) block.Adapter {
	return &Adapter{
		s3:                    a.s3,
		httpClient:            a.httpClient,
		ctx:                   ctx,
		uploadIDTranslator:    a.uploadIDTranslator,
		streamingChunkSize:    a.streamingChunkSize,
		streamingChunkTimeout: a.streamingChunkTimeout,
	}
}

func (a *Adapter) log() logging.Logger {
	return logging.FromContext(a.ctx)
}

func (a *Adapter) Put(obj block.ObjectPointer, sizeBytes int64, reader io.Reader, opts block.PutOpts) error {
	var err error
	defer reportMetrics("Put", time.Now(), &sizeBytes, &err)

	qualifiedKey, err := resolveNamespace(obj)
	if err != nil {
		return err
	}
	putObject := s3.PutObjectInput{
		Bucket:       aws.String(qualifiedKey.StorageNamespace),
		Key:          aws.String(qualifiedKey.Key),
		StorageClass: opts.StorageClass,
	}
	sdkRequest, _ := a.s3.PutObjectRequest(&putObject)
	_, err = a.streamToS3(sdkRequest, sizeBytes, reader)
	return err
}

func (a *Adapter) UploadPart(obj block.ObjectPointer, sizeBytes int64, reader io.Reader, uploadID string, partNumber int64) (string, error) {
	var err error
	defer reportMetrics("UploadPart", time.Now(), &sizeBytes, &err)
	qualifiedKey, err := resolveNamespace(obj)
	if err != nil {
		return "", err
	}
	uploadID = a.uploadIDTranslator.TranslateUploadID(uploadID)
	uploadPartObject := s3.UploadPartInput{
		Bucket:     aws.String(qualifiedKey.StorageNamespace),
		Key:        aws.String(qualifiedKey.Key),
		PartNumber: aws.Int64(partNumber),
		UploadId:   aws.String(uploadID),
	}
	sdkRequest, _ := a.s3.UploadPartRequest(&uploadPartObject)
	etag, err := a.streamToS3(sdkRequest, sizeBytes, reader)
	if err != nil {
		return "", err
	}
	if etag == "" {
		return "", ErrMissingETag
	}
	return etag, nil
}

func (a *Adapter) streamToS3(sdkRequest *request.Request, sizeBytes int64, reader io.Reader) (string, error) {
	sigTime := time.Now()
	log := a.log().WithField("operation", "PutObject")

	if err := sdkRequest.Build(); err != nil {
		return "", err
	}

	req, err := http.NewRequest(sdkRequest.HTTPRequest.Method, sdkRequest.HTTPRequest.URL.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Encoding", StreamingContentEncoding)
	req.Header.Set("Transfer-Encoding", "chunked")
	req.Header.Set("x-amz-content-sha256", StreamingSha256)
	req.Header.Set("x-amz-decoded-content-length", fmt.Sprintf("%d", sizeBytes))
	req.Header.Set("Expect", "100-Continue")

	baseSigner := v4.NewSigner(sdkRequest.Config.Credentials)
	baseSigner.DisableURIPathEscaping = true
	_, err = baseSigner.Sign(req, nil, s3.ServiceName, aws.StringValue(sdkRequest.Config.Region), sigTime)
	if err != nil {
		log.WithError(err).Error("failed to sign request")
		return "", err
	}

	sigSeed, err := v4.GetSignedRequestSignature(req)
	if err != nil {
		log.WithError(err).Error("failed to get seed signature")
		return "", err
	}

	req.Body = ioutil.NopCloser(&StreamingReader{
		Reader: reader,
		Size:   int(sizeBytes),
		Time:   sigTime,
		StreamSigner: v4.NewStreamSigner(
			aws.StringValue(sdkRequest.Config.Region),
			s3.ServiceName,
			sigSeed,
			sdkRequest.Config.Credentials,
		),
		ChunkSize:    a.streamingChunkSize,
		ChunkTimeout: a.streamingChunkTimeout,
	})
	resp, err := a.httpClient.Do(req)
	if err != nil {
		log.WithError(err).
			WithField("url", sdkRequest.HTTPRequest.URL.String()).
			Error("error making request request")
		return "", err
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			err = fmt.Errorf("%w: %d %s (unknown)", ErrS3, resp.StatusCode, resp.Status)
		} else {
			err = fmt.Errorf("%w: %s", ErrS3, body)
		}
		log.WithError(err).
			WithField("url", sdkRequest.HTTPRequest.URL.String()).
			WithField("status_code", resp.StatusCode).
			Error("bad S3 PutObject response")
		return "", err
	}
	etag := resp.Header.Get("Etag")
	// error in case etag is missing - note that empty header value will cause the same error
	if len(etag) == 0 {
		return "", ErrMissingETag
	}
	return etag, nil
}

func (a *Adapter) Get(obj block.ObjectPointer, _ int64) (io.ReadCloser, error) {
	var err error
	var sizeBytes int64
	defer reportMetrics("Get", time.Now(), &sizeBytes, &err)
	qualifiedKey, err := resolveNamespace(obj)
	if err != nil {
		return nil, err
	}
	log := a.log().WithField("operation", "GetObject")
	getObjectInput := s3.GetObjectInput{
		Bucket: aws.String(qualifiedKey.StorageNamespace),
		Key:    aws.String(qualifiedKey.Key),
	}
	objectOutput, err := a.s3.GetObject(&getObjectInput)
	if err != nil {
		log.WithError(err).Error("failed to get S3 object")
		return nil, err
	}
	sizeBytes = *objectOutput.ContentLength
	return objectOutput.Body, nil
}

func (a *Adapter) Exists(obj block.ObjectPointer) (bool, error) {
	var err error
	defer reportMetrics("Exists", time.Now(), nil, &err)
	qualifiedKey, err := resolveNamespace(obj)
	if err != nil {
		return false, err
	}
	log := a.log().WithField("operation", "HeadObject")
	input := s3.HeadObjectInput{
		Bucket: aws.String(qualifiedKey.StorageNamespace),
		Key:    aws.String(qualifiedKey.Key),
	}
	_, err = a.s3.HeadObject(&input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			if aerr.Code() == s3.ErrCodeNoSuchKey {
				return false, nil
			}
		}

		log.WithError(err).Errorf("failed to stat S3 object")
		return false, err
	}
	return true, nil
}

func (a *Adapter) GetRange(obj block.ObjectPointer, startPosition int64, endPosition int64) (io.ReadCloser, error) {
	var err error
	var sizeBytes int64
	defer reportMetrics("GetRange", time.Now(), &sizeBytes, &err)
	qualifiedKey, err := resolveNamespace(obj)
	if err != nil {
		return nil, err
	}
	log := a.log().WithField("operation", "GetObjectRange")
	getObjectInput := s3.GetObjectInput{
		Bucket: aws.String(qualifiedKey.StorageNamespace),
		Key:    aws.String(qualifiedKey.Key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", startPosition, endPosition)),
	}
	objectOutput, err := a.s3.GetObject(&getObjectInput)
	if err != nil {
		log.WithError(err).WithFields(logging.Fields{
			"start_position": startPosition,
			"end_position":   endPosition,
		}).Error("failed to get S3 object range")
		return nil, err
	}
	sizeBytes = *objectOutput.ContentLength
	return objectOutput.Body, nil
}

func (a *Adapter) List(lsOpt block.ListOpts) ([]string, error) {
	log := a.log().WithField("operation", "ListObjects")
	var err error
	var lenRes int64
	defer reportMetrics("GetRange", time.Now(), &lenRes, &err)

	qualifiedPrefix, err := resolveNamespacePrefix(lsOpt)
	if err != nil {
		return nil, err
	}

	listObjectInput := s3.ListObjectsInput{
		Bucket: aws.String(qualifiedPrefix.StorageNamespace),
		Prefix: aws.String(qualifiedPrefix.Prefix),
	}
	listOutput, err := a.s3.ListObjects(&listObjectInput)
	if err != nil {
		log.WithError(err).WithFields(logging.Fields{
			"bucket": qualifiedPrefix.StorageNamespace,
			"prefix": qualifiedPrefix.Prefix,
		}).Error("failed to list S3 objects")
		return nil, err
	}

	var keys []string
	for _, obj := range listOutput.Contents {
		keys = append(keys, *obj.Key)
	}
	lenRes = int64(len(keys))

	return keys, nil
}

func (a *Adapter) GetProperties(obj block.ObjectPointer) (block.Properties, error) {
	var err error
	defer reportMetrics("GetProperties", time.Now(), nil, &err)
	qualifiedKey, err := resolveNamespace(obj)
	if err != nil {
		return block.Properties{}, err
	}
	headObjectParams := &s3.HeadObjectInput{
		Bucket: aws.String(qualifiedKey.StorageNamespace),
		Key:    aws.String(qualifiedKey.Key),
	}
	s3Props, err := a.s3.HeadObject(headObjectParams)
	if err != nil {
		return block.Properties{}, err
	}
	return block.Properties{StorageClass: s3Props.StorageClass}, nil
}

func (a *Adapter) Remove(obj block.ObjectPointer) error {
	var err error
	defer reportMetrics("Remove", time.Now(), nil, &err)
	qualifiedKey, err := resolveNamespace(obj)
	if err != nil {
		return err
	}
	deleteObjectParams := &s3.DeleteObjectInput{
		Bucket: aws.String(qualifiedKey.StorageNamespace),
		Key:    aws.String(qualifiedKey.Key),
	}
	_, err = a.s3.DeleteObject(deleteObjectParams)
	if err != nil {
		a.log().WithError(err).Error("failed to delete S3 object")
		return err
	}
	err = a.s3.WaitUntilObjectNotExists(&s3.HeadObjectInput{
		Bucket: aws.String(qualifiedKey.StorageNamespace),
		Key:    aws.String(qualifiedKey.Key),
	})
	return err
}

func (a *Adapter) Copy(sourceObj, destinationObj block.ObjectPointer) error {
	var err error
	defer reportMetrics("Copy", time.Now(), nil, &err)

	qualifiedDestinationKey, err := resolveNamespace(destinationObj)
	if err != nil {
		return err
	}
	qualifiedSourceKey, err := resolveNamespace(sourceObj)
	if err != nil {
		return err
	}
	copyObjectParams := &s3.CopyObjectInput{
		Bucket:     aws.String(qualifiedDestinationKey.StorageNamespace),
		Key:        aws.String(qualifiedDestinationKey.Key),
		CopySource: aws.String(qualifiedSourceKey.StorageNamespace + "/" + qualifiedSourceKey.Key),
	}
	_, err = a.s3.CopyObject(copyObjectParams)
	if err != nil {
		a.log().WithError(err).Error("failed to copy S3 object")
	}
	return err
}

func (a *Adapter) CreateMultiPartUpload(obj block.ObjectPointer, r *http.Request, opts block.CreateMultiPartUploadOpts) (string, error) {
	var err error
	defer reportMetrics("CreateMultiPartUpload", time.Now(), nil, &err)
	qualifiedKey, err := resolveNamespace(obj)
	if err != nil {
		return "", err
	}
	input := &s3.CreateMultipartUploadInput{
		Bucket:       aws.String(qualifiedKey.StorageNamespace),
		Key:          aws.String(qualifiedKey.Key),
		ContentType:  aws.String(""),
		StorageClass: opts.StorageClass,
	}
	resp, err := a.s3.CreateMultipartUpload(input)
	if err != nil {
		return "", err
	}
	uploadID := *resp.UploadId
	uploadID = a.uploadIDTranslator.SetUploadID(uploadID)
	a.log().WithFields(logging.Fields{
		"upload_id":            *resp.UploadId,
		"translated_upload_id": uploadID,
		"qualified_ns":         qualifiedKey.StorageNamespace,
		"qualified_key":        qualifiedKey.Key,
		"key":                  obj.Identifier,
	}).Debug("created multipart upload")
	return uploadID, err
}

func (a *Adapter) AbortMultiPartUpload(obj block.ObjectPointer, uploadID string) error {
	var err error
	defer reportMetrics("AbortMultiPartUpload", time.Now(), nil, &err)
	qualifiedKey, err := resolveNamespace(obj)
	if err != nil {
		return err
	}
	uploadID = a.uploadIDTranslator.TranslateUploadID(uploadID)
	input := &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(qualifiedKey.StorageNamespace),
		Key:      aws.String(qualifiedKey.Key),
		UploadId: aws.String(uploadID),
	}
	_, err = a.s3.AbortMultipartUpload(input)
	a.uploadIDTranslator.RemoveUploadID(uploadID)
	a.log().WithFields(logging.Fields{
		"upload_id":     uploadID,
		"qualified_ns":  qualifiedKey.StorageNamespace,
		"qualified_key": qualifiedKey.Key,
		"key":           obj.Identifier,
	}).Debug("aborted multipart upload")
	return err
}

func (a *Adapter) CompleteMultiPartUpload(obj block.ObjectPointer, uploadID string, multipartList *block.MultipartUploadCompletion) (*string, int64, error) {
	var err error
	defer reportMetrics("CompleteMultiPartUpload", time.Now(), nil, &err)
	qualifiedKey, err := resolveNamespace(obj)
	if err != nil {
		return nil, 0, err
	}
	cmpu := &s3.CompletedMultipartUpload{Parts: multipartList.Part}
	translatedUploadID := a.uploadIDTranslator.TranslateUploadID(uploadID)
	input := &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(qualifiedKey.StorageNamespace),
		Key:             aws.String(qualifiedKey.Key),
		UploadId:        aws.String(translatedUploadID),
		MultipartUpload: cmpu,
	}
	lg := a.log().WithFields(logging.Fields{
		"upload_id":            uploadID,
		"translated_upload_id": translatedUploadID,
		"qualified_ns":         qualifiedKey.StorageNamespace,
		"qualified_key":        qualifiedKey.Key,
		"key":                  obj.Identifier,
	})
	resp, err := a.s3.CompleteMultipartUpload(input)

	if err != nil {
		lg.WithError(err).Error("CompleteMultipartUpload failed")
		return nil, -1, err
	}
	lg.Debug("completed multipart upload")
	a.uploadIDTranslator.RemoveUploadID(translatedUploadID)
	headInput := &s3.HeadObjectInput{Bucket: &qualifiedKey.StorageNamespace, Key: &qualifiedKey.Key}
	headResp, err := a.s3.HeadObject(headInput)
	if err != nil {
		return nil, -1, err
	} else {
		return resp.ETag, *headResp.ContentLength, err
	}
}

func contains(tags []*s3.Tag, pred func(string, string) bool) bool {
	for _, tag := range tags {
		if pred(*tag.Key, *tag.Value) {
			return true
		}
	}
	return false
}

func isExpirationRule(rule s3.LifecycleRule) bool {
	return rule.Expiration != nil && // Check for *any* expiration -- not its details.
		rule.Status != nil && *rule.Status == "Enabled" &&
		rule.Filter != nil &&
		rule.Filter.Tag != nil &&
		*rule.Filter.Tag.Key == ExpireObjectS3Tag &&
		*rule.Filter.Tag.Value == "1" ||
		rule.Filter.And != nil &&
			contains(rule.Filter.And.Tags,
				func(key string, value string) bool {
					return key == ExpireObjectS3Tag && value == "1"
				},
			)
}

// ValidateConfiguration on an S3 adapter checks for a usable bucket
// lifecycle policy: the storageNamespace bucket should expire objects
// marked with ExpireObjectS3Tag (with _some_ duration, even if
// nonzero).
func (a *Adapter) ValidateConfiguration(storageNamespace string) error {
	getLifecycleConfigInput := &s3.GetBucketLifecycleConfigurationInput{Bucket: &storageNamespace}
	config, err := a.s3.GetBucketLifecycleConfiguration(getLifecycleConfigInput)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok && aerr.Code() == "NoSuchLifecycleConfiguration" {
			return fmt.Errorf("%w: bucket %s has no lifecycle configuration", ErrS3, storageNamespace)
		}
		return err
	}
	a.log().WithFields(logging.Fields{
		"Bucket":          storageNamespace,
		"LifecyclePolicy": config.GoString(),
	}).Info("S3 bucket lifecycle policy")

	hasMatchingRule := false
	for _, a := range config.Rules {
		if isExpirationRule(*a) {
			hasMatchingRule = true
			break
		}
	}

	if !hasMatchingRule {
		return fmt.Errorf("%w: bucket %s lifecycle rules not configured to expire objects tagged \"%s\"",
			ErrS3, storageNamespace, ExpireObjectS3Tag)
	}
	return nil
}

func (a *Adapter) BlockstoreType() string {
	return BlockstoreType
}
