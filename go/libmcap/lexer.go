package libmcap

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

var (
	ErrNestedChunk = errors.New("detected nested chunk")
	ErrBadMagic    = errors.New("not an mcap file")
)

const (
	TokenHeader TokenType = iota
	TokenFooter
	TokenSchema
	TokenChannel
	TokenMessage
	TokenChunk
	TokenMessageIndex
	TokenChunkIndex
	TokenAttachment
	TokenAttachmentIndex
	TokenStatistics
	TokenMetadata
	TokenMetadataIndex
	TokenSummaryOffset
	TokenDataEnd
	TokenError
)

type TokenType int

func (t TokenType) String() string {
	switch t {
	case TokenHeader:
		return "header"
	case TokenFooter:
		return "footer"
	case TokenSchema:
		return "schema"
	case TokenChannel:
		return "channel"
	case TokenMessage:
		return "message"
	case TokenChunk:
		return "chunk"
	case TokenMessageIndex:
		return "message index"
	case TokenChunkIndex:
		return "chunk index"
	case TokenAttachment:
		return "attachment"
	case TokenAttachmentIndex:
		return "attachment index"
	case TokenStatistics:
		return "statistics"
	case TokenMetadata:
		return "metadata"
	case TokenSummaryOffset:
		return "summary offset"
	case TokenDataEnd:
		return "data end"
	case TokenError:
		return "error"
	default:
		return "unknown"
	}
}

type decoders struct {
	lz4  *lz4.Reader
	zstd *zstd.Decoder
	none *bytes.Reader
}

type Lexer struct {
	basereader io.Reader
	reader     io.Reader
	emitChunks bool

	decoders    decoders
	inChunk     bool
	buf         []byte
	validateCRC bool
}

func validateMagic(r io.Reader) error {
	magic := make([]byte, len(Magic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return ErrBadMagic
	}
	if !bytes.Equal(magic, Magic) {
		return ErrBadMagic
	}
	return nil
}

func (l *Lexer) setNoneDecoder(buf []byte) {
	if l.decoders.none == nil {
		l.decoders.none = bytes.NewReader(buf)
	} else {
		l.decoders.none.Reset(buf)
	}
	l.reader = l.decoders.none
}

func (l *Lexer) setLZ4Decoder(r io.Reader) {
	if l.decoders.lz4 == nil {
		l.decoders.lz4 = lz4.NewReader(r)
	} else {
		l.decoders.lz4.Reset(r)
	}
	l.reader = l.decoders.lz4
}

func (l *Lexer) setZSTDDecoder(r io.Reader) error {
	if l.decoders.zstd == nil {
		decoder, err := zstd.NewReader(r)
		if err != nil {
			return err
		}
		l.decoders.zstd = decoder
	} else {
		err := l.decoders.zstd.Reset(r)
		if err != nil {
			return err
		}
	}
	l.reader = l.decoders.zstd
	return nil
}

func loadChunk(l *Lexer) error {
	if l.inChunk {
		return ErrNestedChunk
	}
	_, err := io.ReadFull(l.reader, l.buf[:8+8+8+4+4])
	if err != nil {
		return err
	}

	// the reader does not care about the start, end, or uncompressed size, or
	// they would be using emitChunks.

	// Skip the uncompressed size; the lexer will read messages out of the
	// reader incrementally.
	_, offset, err := getUint64(l.buf, 0) // start
	if err != nil {
		return fmt.Errorf("failed to read start: %w", err)
	}
	_, offset, err = getUint64(l.buf, offset) // end
	if err != nil {
		return fmt.Errorf("failed to read end: %w", err)
	}
	_, offset, err = getUint64(l.buf, offset) // uncompressed size
	if err != nil {
		return fmt.Errorf("failed to read uncompressed size: %w", err)
	}
	uncompressedCRC, offset, err := getUint32(l.buf, offset)
	if err != nil {
		return fmt.Errorf("failed to read uncompressed CRC: %w", err)
	}
	compressionLen, _, err := getUint32(l.buf, offset)
	if err != nil {
		return fmt.Errorf("failed to read compression length: %w", err)
	}

	// read compression and records length into buffer
	_, err = io.ReadFull(l.reader, l.buf[:compressionLen+8])
	if err != nil {
		return fmt.Errorf("failed to read compression from chunk: %w", err)
	}
	compression := CompressionFormat(l.buf[:compressionLen])
	recordsLength, _, err := getUint64(l.buf, int(compressionLen))
	if err != nil {
		return fmt.Errorf("failed to read records length: %w", err)
	}

	// remaining bytes in the record are the chunk data
	lr := io.LimitReader(l.reader, int64(recordsLength))
	switch compression {
	case CompressionNone:
		l.reader = lr
	case CompressionLZ4:
		l.setLZ4Decoder(lr)
	case CompressionZSTD:
		err = l.setZSTDDecoder(lr)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported compression: %s", string(compression))
	}

	// if we are validating the CRC, we need to fully decompress the chunk right
	// here, then rewrap the decompressed data in a compatible reader after
	// validation. If we are not validating CRCs, we can use incremental
	// decompression for the chunk's data, which may be beneficial to streaming
	// readers.
	if l.validateCRC {
		uncompressed, err := io.ReadAll(l.reader)
		if err != nil {
			return err
		}
		crc := crc32.ChecksumIEEE(uncompressed)
		if crc != uncompressedCRC {
			return fmt.Errorf("invalid CRC: %x != %x", crc, uncompressedCRC)
		}
		l.setNoneDecoder(uncompressed)
	}
	l.inChunk = true
	return nil
}

// Next returns the next token from the lexer as a byte array. The result will
// be sliced out of the provided buffer `p`, if p has adequate space. If p does
// not have adequate space, a new buffer with sufficient size is allocated for
// the result.
func (l *Lexer) Next(p []byte) (TokenType, []byte, error) {
	for {
		_, err := io.ReadFull(l.reader, l.buf[:9])
		if err != nil {
			unexpectedEOF := errors.Is(err, io.ErrUnexpectedEOF)
			eof := errors.Is(err, io.EOF)
			if l.inChunk && (eof || unexpectedEOF) {
				l.inChunk = false
				l.reader = l.basereader
				continue
			}
			if unexpectedEOF || eof {
				return TokenError, nil, io.EOF
			}
			return TokenError, nil, err
		}
		opcode := OpCode(l.buf[0])
		recordLen := int64(binary.LittleEndian.Uint64(l.buf[1:9]))

		if opcode == OpChunk && !l.emitChunks {
			err := loadChunk(l)
			if err != nil {
				return TokenError, nil, err
			}
			continue
		}

		if recordLen > int64(len(p)) {
			p = make([]byte, recordLen)
		}

		record := p[:recordLen]
		_, err = io.ReadFull(l.reader, record)
		if err != nil {
			return TokenError, nil, err
		}

		switch opcode {
		case OpHeader:
			return TokenHeader, record, nil
		case OpSchema:
			return TokenSchema, record, nil
		case OpDataEnd:
			return TokenDataEnd, record, nil
		case OpChannel:
			return TokenChannel, record, nil
		case OpFooter:
			return TokenFooter, record, nil
		case OpMessage:
			return TokenMessage, record, nil
		case OpAttachment:
			return TokenAttachment, record, nil
		case OpAttachmentIndex:
			return TokenAttachmentIndex, record, nil
		case OpChunkIndex:
			return TokenChunkIndex, record, nil
		case OpStatistics:
			return TokenStatistics, record, nil
		case OpMessageIndex:
			return TokenMessageIndex, record, nil
		case OpChunk:
			return TokenChunk, record, nil
		case OpMetadata:
			return TokenMetadata, record, nil
		case OpMetadataIndex:
			return TokenMetadataIndex, record, nil
		case OpSummaryOffset:
			return TokenSummaryOffset, record, nil
		case OpInvalidZero:
			return TokenError, nil, fmt.Errorf("invalid zero opcode")
		default:
			continue // skip unrecognized opcodes
		}
	}
}

type LexOpts struct {
	SkipMagic   bool
	ValidateCRC bool
	EmitChunks  bool
}

func NewLexer(r io.Reader, opts ...*LexOpts) (*Lexer, error) {
	var validateCRC, emitChunks, skipMagic bool
	if len(opts) > 0 {
		validateCRC = opts[0].ValidateCRC
		emitChunks = opts[0].EmitChunks
		skipMagic = opts[0].SkipMagic
	}

	if !skipMagic {
		err := validateMagic(r)
		if err != nil {
			return nil, err
		}
	}
	return &Lexer{
		basereader:  r,
		reader:      r,
		buf:         make([]byte, 32),
		validateCRC: validateCRC,
		emitChunks:  emitChunks,
	}, nil
}
