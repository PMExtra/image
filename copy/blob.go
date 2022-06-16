package copy

import (
	"context"
	"io"

	"github.com/containers/image/v5/internal/private"
	compressiontypes "github.com/containers/image/v5/pkg/compression/types"
	"github.com/containers/image/v5/types"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// copyBlobFromStream copies a blob with srcInfo (with known Digest and Annotations and possibly known Size) from srcReader to dest,
// perhaps sending a copy to an io.Writer if getOriginalLayerCopyWriter != nil,
// perhaps (de/re/)compressing it if canModifyBlob,
// and returns a complete blobInfo of the copied blob.
func (c *copier) copyBlobFromStream(ctx context.Context, srcReader io.Reader, srcInfo types.BlobInfo,
	getOriginalLayerCopyWriter func(decompressor compressiontypes.DecompressorFunc) io.Writer,
	canModifyBlob bool, isConfig bool, toEncrypt bool, bar *progressBar, layerIndex int, emptyLayer bool) (types.BlobInfo, error) {
	if isConfig { // This is guaranteed by the caller, but set it here to be explicit.
		canModifyBlob = false
	}

	// The copying happens through a pipeline of connected io.Readers;
	// that pipeline is built by updating stream.
	// === Input: srcReader
	stream := sourceStream{
		reader: srcReader,
		info:   srcInfo,
	}

	// === Process input through digestingReader to validate against the expected digest.
	// Be paranoid; in case PutBlob somehow managed to ignore an error from digestingReader,
	// use a separate validation failure indicator.
	// Note that for this check we don't use the stronger "validationSucceeded" indicator, because
	// dest.PutBlob may detect that the layer already exists, in which case we don't
	// read stream to the end, and validation does not happen.
	digestingReader, err := newDigestingReader(stream.reader, srcInfo.Digest)
	if err != nil {
		return types.BlobInfo{}, errors.Wrapf(err, "preparing to verify blob %s", srcInfo.Digest)
	}
	stream.reader = digestingReader

	// === Update progress bars
	stream.reader = bar.ProxyReader(stream.reader)

	// === Decrypt the stream, if required.
	decryptionStep, err := c.blobPipelineDecryptionStep(&stream, srcInfo)
	if err != nil {
		return types.BlobInfo{}, err
	}

	// === Detect compression of the input stream.
	// This requires us to “peek ahead” into the stream to read the initial part, which requires us to chain through another io.Reader returned by DetectCompression.
	detectedCompression, err := blobPipelineDetectCompressionStep(&stream, srcInfo)
	if err != nil {
		return types.BlobInfo{}, err
	}

	// === Send a copy of the original, uncompressed, stream, to a separate path if necessary.
	var originalLayerReader io.Reader // DO NOT USE this other than to drain the input if no other consumer in the pipeline has done so.
	if getOriginalLayerCopyWriter != nil {
		stream.reader = io.TeeReader(stream.reader, getOriginalLayerCopyWriter(detectedCompression.decompressor))
		originalLayerReader = stream.reader
	}

	// === Deal with layer compression/decompression if necessary
	// WARNING: If you are adding new reasons to change the blob, update also the OptimizeDestinationImageAlreadyExists
	// short-circuit conditions
	compressionStep, err := c.blobPipelineCompressionStep(&stream, canModifyBlob, detectedCompression)
	if err != nil {
		return types.BlobInfo{}, err
	}
	defer compressionStep.close()

	// === Encrypt the stream for valid mediatypes if ociEncryptConfig provided
	if decryptionStep.decrypting && toEncrypt {
		// If nothing else, we can only set uploadedInfo.CryptoOperation to a single value.
		// Before relaxing this, see the original pull request’s review if there are other reasons to reject this.
		return types.BlobInfo{}, errors.New("Unable to support both decryption and encryption in the same copy")
	}
	encryptionStep, err := c.blobPipelineEncryptionStep(&stream, toEncrypt, srcInfo, decryptionStep)
	if err != nil {
		return types.BlobInfo{}, err
	}

	// === Report progress using the c.progress channel, if required.
	if c.progress != nil && c.progressInterval > 0 {
		progressReader := newProgressReader(
			stream.reader,
			c.progress,
			c.progressInterval,
			srcInfo,
		)
		defer progressReader.reportDone()
		stream.reader = progressReader
	}

	// === Finally, send the layer stream to dest.
	options := private.PutBlobOptions{
		Cache:      c.blobInfoCache,
		IsConfig:   isConfig,
		EmptyLayer: emptyLayer,
	}
	if !isConfig {
		options.LayerIndex = &layerIndex
	}
	uploadedInfo, err := c.dest.PutBlobWithOptions(ctx, &errorAnnotationReader{stream.reader}, stream.info, options)
	if err != nil {
		return types.BlobInfo{}, errors.Wrap(err, "writing blob")
	}

	uploadedInfo.Annotations = stream.info.Annotations

	compressionStep.updateCompressionEdits(&uploadedInfo.CompressionOperation, &uploadedInfo.CompressionAlgorithm, &uploadedInfo.Annotations)
	decryptionStep.updateCryptoOperation(&uploadedInfo.CryptoOperation)
	if err := encryptionStep.updateCryptoOperationAndAnnotations(&uploadedInfo.CryptoOperation, &uploadedInfo.Annotations); err != nil {
		return types.BlobInfo{}, err
	}

	// This is fairly horrible: the writer from getOriginalLayerCopyWriter wants to consume
	// all of the input (to compute DiffIDs), even if dest.PutBlob does not need it.
	// So, read everything from originalLayerReader, which will cause the rest to be
	// sent there if we are not already at EOF.
	if getOriginalLayerCopyWriter != nil {
		logrus.Debugf("Consuming rest of the original blob to satisfy getOriginalLayerCopyWriter")
		_, err := io.Copy(io.Discard, originalLayerReader)
		if err != nil {
			return types.BlobInfo{}, errors.Wrapf(err, "reading input blob %s", srcInfo.Digest)
		}
	}

	if digestingReader.validationFailed { // Coverage: This should never happen.
		return types.BlobInfo{}, errors.Errorf("Internal error writing blob %s, digest verification failed but was ignored", srcInfo.Digest)
	}
	if stream.info.Digest != "" && uploadedInfo.Digest != stream.info.Digest {
		return types.BlobInfo{}, errors.Errorf("Internal error writing blob %s, blob with digest %s saved with digest %s", srcInfo.Digest, stream.info.Digest, uploadedInfo.Digest)
	}
	if digestingReader.validationSucceeded {
		if err := compressionStep.recordValidatedDigestData(c, uploadedInfo, srcInfo, encryptionStep, decryptionStep); err != nil {
			return types.BlobInfo{}, err
		}
	}

	return uploadedInfo, nil
}

// sourceStream encapsulates an input consumed by copyBlobFromStream, in progress of being built.
// This allows handles of individual aspects to build the copy pipeline without _too much_
// specific cooperation by the caller.
//
// We are currently very far from a generalized plug-and-play API for building/consuming the pipeline
// without specific knowledge of various aspects in copyBlobFromStream; that may come one day.
type sourceStream struct {
	reader io.Reader
	info   types.BlobInfo // corresponding to the data available in reader.
}

// errorAnnotationReader wraps the io.Reader passed to PutBlob for annotating the error happened during read.
// These errors are reported as PutBlob errors, so we would otherwise misleadingly attribute them to the copy destination.
type errorAnnotationReader struct {
	reader io.Reader
}

// Read annotates the error happened during read
func (r errorAnnotationReader) Read(b []byte) (n int, err error) {
	n, err = r.reader.Read(b)
	if err != io.EOF {
		return n, errors.Wrapf(err, "happened during read")
	}
	return n, err
}