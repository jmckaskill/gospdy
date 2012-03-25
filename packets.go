package spdy

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	LowPriority     = 3
	HighPriority    = -4
	DefaultPriority = 0

	synStreamCode    = (1 << 31) | 1
	synReplyCode     = (1 << 31) | 2
	rstStreamCode    = (1 << 31) | 3
	settingsCode     = (1 << 31) | 4
	noopCode         = (1 << 31) | 5
	pingCode         = (1 << 31) | 6
	goAwayCode       = (1 << 31) | 7
	headersCode      = (1 << 31) | 8
	windowUpdateCode = (1 << 31) | 9

	finishedFlag       = 1
	compressedFlag     = 2
	unidirectionalFlag = 2

	windowSetting = 5

	headerDictionaryV2 = `optionsgetheadpostputdeletetraceacceptaccept-charsetaccept-encodingaccept-languageauthorizationexpectfromhostif-modified-sinceif-matchif-none-matchif-rangeif-unmodifiedsincemax-forwardsproxy-authorizationrangerefererteuser-agent100101200201202203204205206300301302303304305306307400401402403404405406407408409410411412413414415416417500501502503504505accept-rangesageetaglocationproxy-authenticatepublicretry-afterservervarywarningwww-authenticateallowcontent-basecontent-encodingcache-controlconnectiondatetrailertransfer-encodingupgradeviawarningcontent-languagecontent-lengthcontent-locationcontent-md5content-rangecontent-typeetagexpireslast-modifiedset-cookieMondayTuesdayWednesdayThursdayFridaySaturdaySundayJanFebMarAprMayJunJulAugSepOctNovDecchunkedtext/htmlimage/pngimage/jpgimage/gifapplication/xmlapplication/xhtmltext/plainpublicmax-agecharset=iso-8859-1utf-8gzipdeflateHTTP/1.1statusversionurl` + "\x00"
	compressionLevel   = zlib.BestCompression

	rstSuccess             = 0
	rstProtocolError       = 1
	rstInvalidStream       = 2
	rstRefusedStream       = 3
	rstUnsupportedVersion  = 4
	rstCancel              = 5
	rstInternal            = 6
	rstFlowControlError    = 7
	rstStreamInUse         = 8
	rstStreamAlreadyClosed = 9
	rstCredentials         = 10
	rstFrameTooLarge       = 11
)

var headerDictionaryV3 = []byte{
	0x00, 0x00, 0x00, 0x07, 0x6f, 0x70, 0x74, 0x69, // - - - - o p t i
	0x6f, 0x6e, 0x73, 0x00, 0x00, 0x00, 0x04, 0x68, // o n s - - - - h
	0x65, 0x61, 0x64, 0x00, 0x00, 0x00, 0x04, 0x70, // e a d - - - - p
	0x6f, 0x73, 0x74, 0x00, 0x00, 0x00, 0x03, 0x70, // o s t - - - - p
	0x75, 0x74, 0x00, 0x00, 0x00, 0x06, 0x64, 0x65, // u t - - - - d e
	0x6c, 0x65, 0x74, 0x65, 0x00, 0x00, 0x00, 0x05, // l e t e - - - -
	0x74, 0x72, 0x61, 0x63, 0x65, 0x00, 0x00, 0x00, // t r a c e - - -
	0x06, 0x61, 0x63, 0x63, 0x65, 0x70, 0x74, 0x00, // - a c c e p t -
	0x00, 0x00, 0x0e, 0x61, 0x63, 0x63, 0x65, 0x70, // - - - a c c e p
	0x74, 0x2d, 0x63, 0x68, 0x61, 0x72, 0x73, 0x65, // t - c h a r s e
	0x74, 0x00, 0x00, 0x00, 0x0f, 0x61, 0x63, 0x63, // t - - - - a c c
	0x65, 0x70, 0x74, 0x2d, 0x65, 0x6e, 0x63, 0x6f, // e p t - e n c o
	0x64, 0x69, 0x6e, 0x67, 0x00, 0x00, 0x00, 0x0f, // d i n g - - - -
	0x61, 0x63, 0x63, 0x65, 0x70, 0x74, 0x2d, 0x6c, // a c c e p t - l
	0x61, 0x6e, 0x67, 0x75, 0x61, 0x67, 0x65, 0x00, // a n g u a g e -
	0x00, 0x00, 0x0d, 0x61, 0x63, 0x63, 0x65, 0x70, // - - - a c c e p
	0x74, 0x2d, 0x72, 0x61, 0x6e, 0x67, 0x65, 0x73, // t - r a n g e s
	0x00, 0x00, 0x00, 0x03, 0x61, 0x67, 0x65, 0x00, // - - - - a g e -
	0x00, 0x00, 0x05, 0x61, 0x6c, 0x6c, 0x6f, 0x77, // - - - a l l o w
	0x00, 0x00, 0x00, 0x0d, 0x61, 0x75, 0x74, 0x68, // - - - - a u t h
	0x6f, 0x72, 0x69, 0x7a, 0x61, 0x74, 0x69, 0x6f, // o r i z a t i o
	0x6e, 0x00, 0x00, 0x00, 0x0d, 0x63, 0x61, 0x63, // n - - - - c a c
	0x68, 0x65, 0x2d, 0x63, 0x6f, 0x6e, 0x74, 0x72, // h e - c o n t r
	0x6f, 0x6c, 0x00, 0x00, 0x00, 0x0a, 0x63, 0x6f, // o l - - - - c o
	0x6e, 0x6e, 0x65, 0x63, 0x74, 0x69, 0x6f, 0x6e, // n n e c t i o n
	0x00, 0x00, 0x00, 0x0c, 0x63, 0x6f, 0x6e, 0x74, // - - - - c o n t
	0x65, 0x6e, 0x74, 0x2d, 0x62, 0x61, 0x73, 0x65, // e n t - b a s e
	0x00, 0x00, 0x00, 0x10, 0x63, 0x6f, 0x6e, 0x74, // - - - - c o n t
	0x65, 0x6e, 0x74, 0x2d, 0x65, 0x6e, 0x63, 0x6f, // e n t - e n c o
	0x64, 0x69, 0x6e, 0x67, 0x00, 0x00, 0x00, 0x10, // d i n g - - - -
	0x63, 0x6f, 0x6e, 0x74, 0x65, 0x6e, 0x74, 0x2d, // c o n t e n t -
	0x6c, 0x61, 0x6e, 0x67, 0x75, 0x61, 0x67, 0x65, // l a n g u a g e
	0x00, 0x00, 0x00, 0x0e, 0x63, 0x6f, 0x6e, 0x74, // - - - - c o n t
	0x65, 0x6e, 0x74, 0x2d, 0x6c, 0x65, 0x6e, 0x67, // e n t - l e n g
	0x74, 0x68, 0x00, 0x00, 0x00, 0x10, 0x63, 0x6f, // t h - - - - c o
	0x6e, 0x74, 0x65, 0x6e, 0x74, 0x2d, 0x6c, 0x6f, // n t e n t - l o
	0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x00, 0x00, // c a t i o n - -
	0x00, 0x0b, 0x63, 0x6f, 0x6e, 0x74, 0x65, 0x6e, // - - c o n t e n
	0x74, 0x2d, 0x6d, 0x64, 0x35, 0x00, 0x00, 0x00, // t - m d 5 - - -
	0x0d, 0x63, 0x6f, 0x6e, 0x74, 0x65, 0x6e, 0x74, // - c o n t e n t
	0x2d, 0x72, 0x61, 0x6e, 0x67, 0x65, 0x00, 0x00, // - r a n g e - -
	0x00, 0x0c, 0x63, 0x6f, 0x6e, 0x74, 0x65, 0x6e, // - - c o n t e n
	0x74, 0x2d, 0x74, 0x79, 0x70, 0x65, 0x00, 0x00, // t - t y p e - -
	0x00, 0x04, 0x64, 0x61, 0x74, 0x65, 0x00, 0x00, // - - d a t e - -
	0x00, 0x04, 0x65, 0x74, 0x61, 0x67, 0x00, 0x00, // - - e t a g - -
	0x00, 0x06, 0x65, 0x78, 0x70, 0x65, 0x63, 0x74, // - - e x p e c t
	0x00, 0x00, 0x00, 0x07, 0x65, 0x78, 0x70, 0x69, // - - - - e x p i
	0x72, 0x65, 0x73, 0x00, 0x00, 0x00, 0x04, 0x66, // r e s - - - - f
	0x72, 0x6f, 0x6d, 0x00, 0x00, 0x00, 0x04, 0x68, // r o m - - - - h
	0x6f, 0x73, 0x74, 0x00, 0x00, 0x00, 0x08, 0x69, // o s t - - - - i
	0x66, 0x2d, 0x6d, 0x61, 0x74, 0x63, 0x68, 0x00, // f - m a t c h -
	0x00, 0x00, 0x11, 0x69, 0x66, 0x2d, 0x6d, 0x6f, // - - - i f - m o
	0x64, 0x69, 0x66, 0x69, 0x65, 0x64, 0x2d, 0x73, // d i f i e d - s
	0x69, 0x6e, 0x63, 0x65, 0x00, 0x00, 0x00, 0x0d, // i n c e - - - -
	0x69, 0x66, 0x2d, 0x6e, 0x6f, 0x6e, 0x65, 0x2d, // i f - n o n e -
	0x6d, 0x61, 0x74, 0x63, 0x68, 0x00, 0x00, 0x00, // m a t c h - - -
	0x08, 0x69, 0x66, 0x2d, 0x72, 0x61, 0x6e, 0x67, // - i f - r a n g
	0x65, 0x00, 0x00, 0x00, 0x13, 0x69, 0x66, 0x2d, // e - - - - i f -
	0x75, 0x6e, 0x6d, 0x6f, 0x64, 0x69, 0x66, 0x69, // u n m o d i f i
	0x65, 0x64, 0x2d, 0x73, 0x69, 0x6e, 0x63, 0x65, // e d - s i n c e
	0x00, 0x00, 0x00, 0x0d, 0x6c, 0x61, 0x73, 0x74, // - - - - l a s t
	0x2d, 0x6d, 0x6f, 0x64, 0x69, 0x66, 0x69, 0x65, // - m o d i f i e
	0x64, 0x00, 0x00, 0x00, 0x08, 0x6c, 0x6f, 0x63, // d - - - - l o c
	0x61, 0x74, 0x69, 0x6f, 0x6e, 0x00, 0x00, 0x00, // a t i o n - - -
	0x0c, 0x6d, 0x61, 0x78, 0x2d, 0x66, 0x6f, 0x72, // - m a x - f o r
	0x77, 0x61, 0x72, 0x64, 0x73, 0x00, 0x00, 0x00, // w a r d s - - -
	0x06, 0x70, 0x72, 0x61, 0x67, 0x6d, 0x61, 0x00, // - p r a g m a -
	0x00, 0x00, 0x12, 0x70, 0x72, 0x6f, 0x78, 0x79, // - - - p r o x y
	0x2d, 0x61, 0x75, 0x74, 0x68, 0x65, 0x6e, 0x74, // - a u t h e n t
	0x69, 0x63, 0x61, 0x74, 0x65, 0x00, 0x00, 0x00, // i c a t e - - -
	0x13, 0x70, 0x72, 0x6f, 0x78, 0x79, 0x2d, 0x61, // - p r o x y - a
	0x75, 0x74, 0x68, 0x6f, 0x72, 0x69, 0x7a, 0x61, // u t h o r i z a
	0x74, 0x69, 0x6f, 0x6e, 0x00, 0x00, 0x00, 0x05, // t i o n - - - -
	0x72, 0x61, 0x6e, 0x67, 0x65, 0x00, 0x00, 0x00, // r a n g e - - -
	0x07, 0x72, 0x65, 0x66, 0x65, 0x72, 0x65, 0x72, // - r e f e r e r
	0x00, 0x00, 0x00, 0x0b, 0x72, 0x65, 0x74, 0x72, // - - - - r e t r
	0x79, 0x2d, 0x61, 0x66, 0x74, 0x65, 0x72, 0x00, // y - a f t e r -
	0x00, 0x00, 0x06, 0x73, 0x65, 0x72, 0x76, 0x65, // - - - s e r v e
	0x72, 0x00, 0x00, 0x00, 0x02, 0x74, 0x65, 0x00, // r - - - - t e -
	0x00, 0x00, 0x07, 0x74, 0x72, 0x61, 0x69, 0x6c, // - - - t r a i l
	0x65, 0x72, 0x00, 0x00, 0x00, 0x11, 0x74, 0x72, // e r - - - - t r
	0x61, 0x6e, 0x73, 0x66, 0x65, 0x72, 0x2d, 0x65, // a n s f e r - e
	0x6e, 0x63, 0x6f, 0x64, 0x69, 0x6e, 0x67, 0x00, // n c o d i n g -
	0x00, 0x00, 0x07, 0x75, 0x70, 0x67, 0x72, 0x61, // - - - u p g r a
	0x64, 0x65, 0x00, 0x00, 0x00, 0x0a, 0x75, 0x73, // d e - - - - u s
	0x65, 0x72, 0x2d, 0x61, 0x67, 0x65, 0x6e, 0x74, // e r - a g e n t
	0x00, 0x00, 0x00, 0x04, 0x76, 0x61, 0x72, 0x79, // - - - - v a r y
	0x00, 0x00, 0x00, 0x03, 0x76, 0x69, 0x61, 0x00, // - - - - v i a -
	0x00, 0x00, 0x07, 0x77, 0x61, 0x72, 0x6e, 0x69, // - - - w a r n i
	0x6e, 0x67, 0x00, 0x00, 0x00, 0x10, 0x77, 0x77, // n g - - - - w w
	0x77, 0x2d, 0x61, 0x75, 0x74, 0x68, 0x65, 0x6e, // w - a u t h e n
	0x74, 0x69, 0x63, 0x61, 0x74, 0x65, 0x00, 0x00, // t i c a t e - -
	0x00, 0x06, 0x6d, 0x65, 0x74, 0x68, 0x6f, 0x64, // - - m e t h o d
	0x00, 0x00, 0x00, 0x03, 0x67, 0x65, 0x74, 0x00, // - - - - g e t -
	0x00, 0x00, 0x06, 0x73, 0x74, 0x61, 0x74, 0x75, // - - - s t a t u
	0x73, 0x00, 0x00, 0x00, 0x06, 0x32, 0x30, 0x30, // s - - - - 2 0 0
	0x20, 0x4f, 0x4b, 0x00, 0x00, 0x00, 0x07, 0x76, // - O K - - - - v
	0x65, 0x72, 0x73, 0x69, 0x6f, 0x6e, 0x00, 0x00, // e r s i o n - -
	0x00, 0x08, 0x48, 0x54, 0x54, 0x50, 0x2f, 0x31, // - - H T T P - 1
	0x2e, 0x31, 0x00, 0x00, 0x00, 0x03, 0x75, 0x72, // - 1 - - - - u r
	0x6c, 0x00, 0x00, 0x00, 0x06, 0x70, 0x75, 0x62, // l - - - - p u b
	0x6c, 0x69, 0x63, 0x00, 0x00, 0x00, 0x0a, 0x73, // l i c - - - - s
	0x65, 0x74, 0x2d, 0x63, 0x6f, 0x6f, 0x6b, 0x69, // e t - c o o k i
	0x65, 0x00, 0x00, 0x00, 0x0a, 0x6b, 0x65, 0x65, // e - - - - k e e
	0x70, 0x2d, 0x61, 0x6c, 0x69, 0x76, 0x65, 0x00, // p - a l i v e -
	0x00, 0x00, 0x06, 0x6f, 0x72, 0x69, 0x67, 0x69, // - - - o r i g i
	0x6e, 0x31, 0x30, 0x30, 0x31, 0x30, 0x31, 0x32, // n 1 0 0 1 0 1 2
	0x30, 0x31, 0x32, 0x30, 0x32, 0x32, 0x30, 0x35, // 0 1 2 0 2 2 0 5
	0x32, 0x30, 0x36, 0x33, 0x30, 0x30, 0x33, 0x30, // 2 0 6 3 0 0 3 0
	0x32, 0x33, 0x30, 0x33, 0x33, 0x30, 0x34, 0x33, // 2 3 0 3 3 0 4 3
	0x30, 0x35, 0x33, 0x30, 0x36, 0x33, 0x30, 0x37, // 0 5 3 0 6 3 0 7
	0x34, 0x30, 0x32, 0x34, 0x30, 0x35, 0x34, 0x30, // 4 0 2 4 0 5 4 0
	0x36, 0x34, 0x30, 0x37, 0x34, 0x30, 0x38, 0x34, // 6 4 0 7 4 0 8 4
	0x30, 0x39, 0x34, 0x31, 0x30, 0x34, 0x31, 0x31, // 0 9 4 1 0 4 1 1
	0x34, 0x31, 0x32, 0x34, 0x31, 0x33, 0x34, 0x31, // 4 1 2 4 1 3 4 1
	0x34, 0x34, 0x31, 0x35, 0x34, 0x31, 0x36, 0x34, // 4 4 1 5 4 1 6 4
	0x31, 0x37, 0x35, 0x30, 0x32, 0x35, 0x30, 0x34, // 1 7 5 0 2 5 0 4
	0x35, 0x30, 0x35, 0x32, 0x30, 0x33, 0x20, 0x4e, // 5 0 5 2 0 3 - N
	0x6f, 0x6e, 0x2d, 0x41, 0x75, 0x74, 0x68, 0x6f, // o n - A u t h o
	0x72, 0x69, 0x74, 0x61, 0x74, 0x69, 0x76, 0x65, // r i t a t i v e
	0x20, 0x49, 0x6e, 0x66, 0x6f, 0x72, 0x6d, 0x61, // - I n f o r m a
	0x74, 0x69, 0x6f, 0x6e, 0x32, 0x30, 0x34, 0x20, // t i o n 2 0 4 -
	0x4e, 0x6f, 0x20, 0x43, 0x6f, 0x6e, 0x74, 0x65, // N o - C o n t e
	0x6e, 0x74, 0x33, 0x30, 0x31, 0x20, 0x4d, 0x6f, // n t 3 0 1 - M o
	0x76, 0x65, 0x64, 0x20, 0x50, 0x65, 0x72, 0x6d, // v e d - P e r m
	0x61, 0x6e, 0x65, 0x6e, 0x74, 0x6c, 0x79, 0x34, // a n e n t l y 4
	0x30, 0x30, 0x20, 0x42, 0x61, 0x64, 0x20, 0x52, // 0 0 - B a d - R
	0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x34, 0x30, // e q u e s t 4 0
	0x31, 0x20, 0x55, 0x6e, 0x61, 0x75, 0x74, 0x68, // 1 - U n a u t h
	0x6f, 0x72, 0x69, 0x7a, 0x65, 0x64, 0x34, 0x30, // o r i z e d 4 0
	0x33, 0x20, 0x46, 0x6f, 0x72, 0x62, 0x69, 0x64, // 3 - F o r b i d
	0x64, 0x65, 0x6e, 0x34, 0x30, 0x34, 0x20, 0x4e, // d e n 4 0 4 - N
	0x6f, 0x74, 0x20, 0x46, 0x6f, 0x75, 0x6e, 0x64, // o t - F o u n d
	0x35, 0x30, 0x30, 0x20, 0x49, 0x6e, 0x74, 0x65, // 5 0 0 - I n t e
	0x72, 0x6e, 0x61, 0x6c, 0x20, 0x53, 0x65, 0x72, // r n a l - S e r
	0x76, 0x65, 0x72, 0x20, 0x45, 0x72, 0x72, 0x6f, // v e r - E r r o
	0x72, 0x35, 0x30, 0x31, 0x20, 0x4e, 0x6f, 0x74, // r 5 0 1 - N o t
	0x20, 0x49, 0x6d, 0x70, 0x6c, 0x65, 0x6d, 0x65, // - I m p l e m e
	0x6e, 0x74, 0x65, 0x64, 0x35, 0x30, 0x33, 0x20, // n t e d 5 0 3 -
	0x53, 0x65, 0x72, 0x76, 0x69, 0x63, 0x65, 0x20, // S e r v i c e -
	0x55, 0x6e, 0x61, 0x76, 0x61, 0x69, 0x6c, 0x61, // U n a v a i l a
	0x62, 0x6c, 0x65, 0x4a, 0x61, 0x6e, 0x20, 0x46, // b l e J a n - F
	0x65, 0x62, 0x20, 0x4d, 0x61, 0x72, 0x20, 0x41, // e b - M a r - A
	0x70, 0x72, 0x20, 0x4d, 0x61, 0x79, 0x20, 0x4a, // p r - M a y - J
	0x75, 0x6e, 0x20, 0x4a, 0x75, 0x6c, 0x20, 0x41, // u n - J u l - A
	0x75, 0x67, 0x20, 0x53, 0x65, 0x70, 0x74, 0x20, // u g - S e p t -
	0x4f, 0x63, 0x74, 0x20, 0x4e, 0x6f, 0x76, 0x20, // O c t - N o v -
	0x44, 0x65, 0x63, 0x20, 0x30, 0x30, 0x3a, 0x30, // D e c - 0 0 - 0
	0x30, 0x3a, 0x30, 0x30, 0x20, 0x4d, 0x6f, 0x6e, // 0 - 0 0 - M o n
	0x2c, 0x20, 0x54, 0x75, 0x65, 0x2c, 0x20, 0x57, // - - T u e - - W
	0x65, 0x64, 0x2c, 0x20, 0x54, 0x68, 0x75, 0x2c, // e d - - T h u -
	0x20, 0x46, 0x72, 0x69, 0x2c, 0x20, 0x53, 0x61, // - F r i - - S a
	0x74, 0x2c, 0x20, 0x53, 0x75, 0x6e, 0x2c, 0x20, // t - - S u n - -
	0x47, 0x4d, 0x54, 0x63, 0x68, 0x75, 0x6e, 0x6b, // G M T c h u n k
	0x65, 0x64, 0x2c, 0x74, 0x65, 0x78, 0x74, 0x2f, // e d - t e x t -
	0x68, 0x74, 0x6d, 0x6c, 0x2c, 0x69, 0x6d, 0x61, // h t m l - i m a
	0x67, 0x65, 0x2f, 0x70, 0x6e, 0x67, 0x2c, 0x69, // g e - p n g - i
	0x6d, 0x61, 0x67, 0x65, 0x2f, 0x6a, 0x70, 0x67, // m a g e - j p g
	0x2c, 0x69, 0x6d, 0x61, 0x67, 0x65, 0x2f, 0x67, // - i m a g e - g
	0x69, 0x66, 0x2c, 0x61, 0x70, 0x70, 0x6c, 0x69, // i f - a p p l i
	0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x2f, 0x78, // c a t i o n - x
	0x6d, 0x6c, 0x2c, 0x61, 0x70, 0x70, 0x6c, 0x69, // m l - a p p l i
	0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x2f, 0x78, // c a t i o n - x
	0x68, 0x74, 0x6d, 0x6c, 0x2b, 0x78, 0x6d, 0x6c, // h t m l - x m l
	0x2c, 0x74, 0x65, 0x78, 0x74, 0x2f, 0x70, 0x6c, // - t e x t - p l
	0x61, 0x69, 0x6e, 0x2c, 0x74, 0x65, 0x78, 0x74, // a i n - t e x t
	0x2f, 0x6a, 0x61, 0x76, 0x61, 0x73, 0x63, 0x72, // - j a v a s c r
	0x69, 0x70, 0x74, 0x2c, 0x70, 0x75, 0x62, 0x6c, // i p t - p u b l
	0x69, 0x63, 0x70, 0x72, 0x69, 0x76, 0x61, 0x74, // i c p r i v a t
	0x65, 0x6d, 0x61, 0x78, 0x2d, 0x61, 0x67, 0x65, // e m a x - a g e
	0x3d, 0x67, 0x7a, 0x69, 0x70, 0x2c, 0x64, 0x65, // - g z i p - d e
	0x66, 0x6c, 0x61, 0x74, 0x65, 0x2c, 0x73, 0x64, // f l a t e - s d
	0x63, 0x68, 0x63, 0x68, 0x61, 0x72, 0x73, 0x65, // c h c h a r s e
	0x74, 0x3d, 0x75, 0x74, 0x66, 0x2d, 0x38, 0x63, // t - u t f - 8 c
	0x68, 0x61, 0x72, 0x73, 0x65, 0x74, 0x3d, 0x69, // h a r s e t - i
	0x73, 0x6f, 0x2d, 0x38, 0x38, 0x35, 0x39, 0x2d, // s o - 8 8 5 9 -
	0x31, 0x2c, 0x75, 0x74, 0x66, 0x2d, 0x2c, 0x2a, // 1 - u t f - - -
	0x2c, 0x65, 0x6e, 0x71, 0x3d, 0x30, 0x2e, // - e n q - 0 -
}

func toBig16(d []byte, val uint16) {
	d[0] = byte(val >> 8)
	d[1] = byte(val)
}

func toBig32(d []byte, val uint32) {
	d[0] = byte(val >> 24)
	d[1] = byte(val >> 16)
	d[2] = byte(val >> 8)
	d[3] = byte(val)
}

func toLittle32(d []byte, val uint32) {
	d[0] = byte(val)
	d[1] = byte(val >> 8)
	d[2] = byte(val >> 16)
	d[3] = byte(val >> 24)
}

func fromBig16(d []byte) uint16 {
	return uint16(d[0])<<8 | uint16(d[1])
}

func fromBig32(d []byte) uint32 {
	return uint32(d[0])<<24 |
		uint32(d[1])<<16 |
		uint32(d[2])<<8 |
		uint32(d[3])
}

func fromLittle32(d []byte) uint32 {
	return uint32(d[0]) |
		uint32(d[1])<<8 |
		uint32(d[2])<<16 |
		uint32(d[3])<<24
}

type input struct {
	*bytes.Buffer
}

type decompressor struct {
	in  *bytes.Buffer
	out io.ReadCloser
}

func (s *decompressor) Decompress(streamId int, version int, data []byte) (headers http.Header, err error) {

	if s.in == nil {
		s.in = bytes.NewBuffer(nil)
	}

	s.in.Write(data)

	if s.out == nil {
		switch version {
		case 2:
			s.out, err = zlib.NewReaderDict(s.in, []byte(headerDictionaryV2))
		case 3:
			s.out, err = zlib.NewReaderDict(s.in, headerDictionaryV3)
		default:
			err = errStreamVersion{streamId, version}
		}

		if err != nil {
			return nil, err
		}
	}

	var numkeys int

	switch version {
	case 2:
		h := [2]byte{}
		if _, err := s.out.Read(h[:]); err != nil {
			return nil, err
		}
		numkeys = int(fromBig16(h[:]))
	case 3:
		h := [4]byte{}
		if _, err := s.out.Read(h[:]); err != nil {
			return nil, err
		}
		numkeys = int(fromBig32(h[:]))
	default:
		return nil, errStreamVersion{streamId, version}
	}

	headers = make(http.Header)
	for i := 0; i < numkeys; i++ {
		var klen, vlen int

		// Pull out the key

		switch version {
		case 2:
			h := [2]byte{}
			if _, err := s.out.Read(h[:]); err != nil {
				return nil, err
			}
			klen = int(fromBig16(h[:]))
		case 3:
			h := [4]byte{}
			if _, err := s.out.Read(h[:]); err != nil {
				return nil, err
			}
			klen = int(fromBig32(h[:]))
		default:
			return nil, errStreamVersion{streamId, version}
		}

		if klen < 0 || klen > defaultBufferSize {
			// TODO(james) new error as this isn't the whole packet data
			return nil, errParse(data)
		}

		key := make([]byte, klen)
		if _, err := s.out.Read(key); err != nil {
			return nil, err
		}

		// Pull out the value

		switch version {
		case 2:
			h := [2]byte{}
			if _, err := s.out.Read(h[:]); err != nil {
				return nil, err
			}
			vlen = int(fromBig16(h[:]))
		case 3:
			h := [4]byte{}
			if _, err := s.out.Read(h[:]); err != nil {
				return nil, err
			}
			vlen = int(fromBig32(h[:]))
		default:
			return nil, errStreamVersion{streamId, version}
		}

		if vlen < 0 || vlen > defaultBufferSize {
			// TODO(james) new error as this isn't the whole packet data
			return nil, errParse(data)
		}

		val := make([]byte, vlen)
		if _, err := s.out.Read(val); err != nil {
			return nil, err
		}

		// Split the value on nul boundaries
		for _, val := range bytes.Split(val, []byte{'\x00'}) {
			headers.Add(string(key), string(val))
		}
	}

	return headers, nil
}

type compressor struct {
	buf *bytes.Buffer
	w   *zlib.Writer
}

func (s *compressor) Begin(version int, init []byte, headers http.Header, numkeys int) (err error) {
	if s.buf == nil {
		s.buf = bytes.NewBuffer(make([]byte, 0, len(init)))
		s.buf.Write(init)

		switch version {
		case 2:
			s.w, err = zlib.NewWriterLevelDict(s.buf, compressionLevel, []byte(headerDictionaryV2))
		case 3:
			s.w, err = zlib.NewWriterLevelDict(s.buf, compressionLevel, headerDictionaryV3)
		default:
			err = errSessionVersion(version)
		}

		if err != nil {
			return err
		}

	} else {
		s.buf.Reset()
		s.buf.Write(init)
	}

	switch version {
	case 2:
		var keys [2]byte
		toBig16(keys[:], uint16(len(headers)+numkeys))
		s.w.Write(keys[:])

		for key, val := range headers {
			var k, v [2]byte
			vals := strings.Join(val, "\x00")

			toBig16(k[:], uint16(len(key)))
			s.w.Write(k[:])
			s.w.Write(bytes.ToLower([]byte(key)))

			toBig16(v[:], uint16(len(vals)))
			s.w.Write(v[:])
			s.w.Write([]byte(vals))
		}
	case 3:
		var keys [4]byte
		toBig32(keys[:], uint32(len(headers)+numkeys))
		s.w.Write(keys[:])

		for key, val := range headers {
			var k, v [4]byte

			toBig32(k[:], uint32(len(key)))
			s.w.Write(k[:])
			s.w.Write(bytes.ToLower([]byte(key)))

			vals := strings.Join(val, "\x00")
			toBig32(v[:], uint32(len(vals)))
			s.w.Write(v[:])
			s.w.Write([]byte(vals))
		}
	default:
		return errSessionVersion(version)
	}

	return nil
}

// TODO(james): what to do if len(key) or len(val) > UINT16_MAX or UINT32_MAX

func (s *compressor) CompressV2(key string, val string) {
	var k, v [2]byte
	toBig16(k[:], uint16(len(key)))
	toBig16(v[:], uint16(len(val)))
	s.w.Write(k[:])
	s.w.Write([]byte(key))
	s.w.Write(v[:])
	s.w.Write([]byte(val))
}

func (s *compressor) CompressV3(key string, val string) {
	var k, v [4]byte
	toBig32(k[:], uint32(len(key)))
	toBig32(v[:], uint32(len(val)))
	s.w.Write(k[:])
	s.w.Write([]byte(key))
	s.w.Write(v[:])
	s.w.Write([]byte(val))
}

func (s *compressor) Finish() []byte {
	s.w.Flush()
	return s.buf.Bytes()
}

type frame interface {
	WriteFrame(w io.Writer, c *compressor) error
}

func popHeader(h http.Header, key string) string {
	r := h.Get(key)
	h.Del(key)
	return r
}

type synStreamFrame struct {
	Version            int
	Finished           bool
	Unidirectional     bool
	StreamId           int
	AssociatedStreamId int
	Header             http.Header
	Priority           int
	URL                *url.URL
	Proto              string
	ProtoMajor         int
	ProtoMinor         int
	Method             string
}

var invalidSynStreamHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Connection",
	"Transfer-Encoding",
}

func (s *synStreamFrame) WriteFrame(w io.Writer, c *compressor) error {
	Log("tx SYN_STREAM %+v\n", *s)

	flags := uint32(0)
	if s.Finished {
		flags |= finishedFlag << 24
	}
	if s.Unidirectional {
		flags |= unidirectionalFlag << 24
	}

	if s.Header != nil {
		for _, key := range invalidSynStreamHeaders {
			delete(s.Header, key)
		}
	}

	if err := c.Begin(s.Version, make([]byte, 18), s.Header, 5); err != nil {
		return err
	}

	path := s.URL.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if s.URL.RawQuery != "" {
		path += "?" + s.URL.RawQuery
	}

	switch s.Version {
	case 2:
		c.CompressV2("version", s.Proto)
		c.CompressV2("method", s.Method)
		c.CompressV2("url", path)
		c.CompressV2("host", s.URL.Host)
		c.CompressV2("scheme", s.URL.Scheme)
	case 3:
		c.CompressV3(":version", s.Proto)
		c.CompressV3(":method", s.Method)
		c.CompressV3(":path", path)
		c.CompressV3(":host", s.URL.Host)
		c.CompressV3(":scheme", s.URL.Scheme)
	default:
		return errStreamVersion{s.StreamId, s.Version}
	}

	h := c.Finish()
	toBig32(h[0:], synStreamCode|uint32(s.Version<<16))
	toBig32(h[4:], flags|uint32(len(h)-8))
	toBig32(h[8:], uint32(s.StreamId))
	toBig32(h[12:], uint32(s.AssociatedStreamId))
	// Priority is 2 bits in V2, this works correctly in that case because
	// in V2 we don't use the bottom priority bit.
	h[16] = byte((s.Priority - HighPriority) << 5)
	h[17] = 0 // unused

	_, err := w.Write(h)
	return err
}

func parseSynStream(d []byte, c *decompressor) (*synStreamFrame, error) {
	if len(d) < 12 {
		Log("invalid SYN_STREAM length\n")
		return nil, errParse(d)
	}

	sid := int(fromBig32(d[8:]))

	if len(d) < 18 {
		Log("invalid SYN_STREAM length\n")
		return nil, errStreamProtocol(sid)
	} else if sid < 0 {
		Log("invalid stream id\n")
		return nil, errStreamProtocol(sid)
	}

	s := &synStreamFrame{
		Version:            int(fromBig16(d[0:]) & 0x7FFF),
		Finished:           (d[4] & finishedFlag) != 0,
		Unidirectional:     (d[4] & unidirectionalFlag) != 0,
		StreamId:           sid,
		AssociatedStreamId: int(fromBig32(d[12:])),
		Priority:           int(d[16]>>5) + HighPriority,
	}

	if s.AssociatedStreamId < 0 {
		Log("invalid associated stream id\n")
		return nil, errStreamProtocol(sid)
	}

	var err error
	if s.Header, err = c.Decompress(s.StreamId, s.Version, d[18:]); err != nil {
		Log("SYN_STREAM decompress error: %v\n", err)
		return nil, err
	}

	var scheme, host, path string

	switch s.Version {
	case 2:
		s.Proto = popHeader(s.Header, "Version")
		s.Method = popHeader(s.Header, "Method")
		scheme = popHeader(s.Header, "Scheme")
		host = popHeader(s.Header, "Host")
		path = popHeader(s.Header, "Url")
	case 3:
		s.Proto = popHeader(s.Header, ":version")
		s.Method = popHeader(s.Header, ":method")
		scheme = popHeader(s.Header, ":scheme")
		host = popHeader(s.Header, ":host")
		path = popHeader(s.Header, ":path")
	default:
		Log("SYN_STREAM unsupported version %d\n", s.Version)
		return nil, errStreamVersion{sid, s.Version}
	}

	var ok bool
	if s.ProtoMajor, s.ProtoMinor, ok = http.ParseHTTPVersion(s.Proto); !ok {
		Log("SYN_STREAM could not parse http version %s\n", s.Proto)
		return nil, errStreamProtocol(sid)
	}

	s.URL, err = url.Parse(fmt.Sprintf("%s://%s%s", scheme, host, path))
	if err != nil || strings.Index(scheme, ":") >= 0 || strings.Index(host, "/") >= 0 || len(path) == 0 || path[0] != '/' {
		Log("invalid SYN_STREAM url %s://%s%s: %v\n", scheme, host, path, err)
		return nil, errStreamProtocol(sid)
	}

	for _, key := range invalidSynStreamHeaders {
		if s.Header[key] != nil {
			Log("invalid SYN_STREAM header %s: %s\n", key, s.Header.Get(key))
			return nil, errStreamProtocol(sid)
		}
	}

	if len(s.Header) == 0 {
		s.Header = nil
	}

	return s, nil
}

type synReplyFrame struct {
	Version    int
	Finished   bool
	StreamId   int
	Header     http.Header
	Status     string
	Proto      string
	ProtoMajor int
	ProtoMinor int
}

var invalidSynReplyHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Connection",
	"Transfer-Encoding",
}

func (s *synReplyFrame) WriteFrame(w io.Writer, c *compressor) error {
	Log("tx SYN_REPLY %+v\n", *s)

	flags := uint32(0)
	if s.Finished {
		flags |= finishedFlag << 24
	}

	if s.Header != nil {
		for _, key := range invalidSynReplyHeaders {
			delete(s.Header, key)
		}
	}

	switch s.Version {
	case 2:
		if err := c.Begin(s.Version, make([]byte, 14), s.Header, 2); err != nil {
			return err
		}
		c.CompressV2("status", s.Status)
		c.CompressV2("version", s.Proto)
	case 3:
		if err := c.Begin(s.Version, make([]byte, 12), s.Header, 2); err != nil {
			return err
		}
		c.CompressV3(":status", s.Status)
		c.CompressV3(":version", s.Proto)
	default:
		return errSessionVersion(s.Version)
	}

	h := c.Finish()
	toBig32(h[0:], synReplyCode|uint32(s.Version<<16))
	toBig32(h[4:], flags|uint32(len(h)-8))
	toBig32(h[8:], uint32(s.StreamId))

	_, err := w.Write(h)
	return err
}

func parseSynReply(d []byte, c *decompressor) (*synReplyFrame, error) {
	if len(d) < 12 {
		return nil, errParse(d)
	}

	s := &synReplyFrame{
		Version:  int(fromBig16(d[0:]) & 0x7FFF),
		Finished: (d[4] & finishedFlag) != 0,
		StreamId: int(fromBig32(d[8:])),
	}

	if s.StreamId < 0 {
		return nil, errStreamProtocol(s.StreamId)
	}

	var err error
	switch s.Version {
	case 2:
		if len(d) < 14 {
			return nil, errStreamProtocol(s.StreamId)
		}
		if s.Header, err = c.Decompress(s.StreamId, s.Version, d[14:]); err != nil {
			return nil, err
		}
		s.Status = popHeader(s.Header, "Status")
		s.Proto = popHeader(s.Header, "Version")

	case 3:
		if s.Header, err = c.Decompress(s.StreamId, s.Version, d[12:]); err != nil {
			return nil, err
		}
		s.Status = popHeader(s.Header, ":status")
		s.Proto = popHeader(s.Header, ":version")

	default:
		return nil, errStreamVersion{s.StreamId, s.Version}
	}

	if len(s.Status) == 0 || len(s.Proto) == 0 {
		return nil, errStreamProtocol(s.StreamId)
	}

	var ok bool
	if s.ProtoMajor, s.ProtoMinor, ok = http.ParseHTTPVersion(s.Proto); !ok {
		return nil, errStreamProtocol(s.StreamId)
	}

	for _, key := range invalidSynReplyHeaders {
		if s.Header[key] != nil {
			Log("invalid SYN_REPLY header %s: %s\n", key, s.Header.Get(key))
			return nil, errStreamProtocol(s.StreamId)
		}
	}

	if len(s.Header) == 0 {
		s.Header = nil
	}

	return s, nil
}

type headersFrame struct {
	Version  int
	Finished bool
	StreamId int
	Header   http.Header
}

func (s *headersFrame) WriteFrame(w io.Writer, c *compressor) error {
	Log("tx HEADERS %+v\n", *s)

	flags := uint32(0)
	if s.Finished {
		flags |= finishedFlag << 24
	}

	switch s.Version {
	case 2:
		if err := c.Begin(s.Version, make([]byte, 14), s.Header, 0); err != nil {
			return err
		}
	case 3:
		if err := c.Begin(s.Version, make([]byte, 12), s.Header, 0); err != nil {
			return err
		}
	default:
		return errSessionVersion(s.Version)
	}

	h := c.Finish()
	toBig32(h[0:], headersCode|uint32(s.Version<<16))
	toBig32(h[4:], flags|uint32(len(h)-8))
	toBig32(h[8:], uint32(s.StreamId))

	_, err := w.Write(h)
	return err
}

func parseHeaders(d []byte, c *decompressor) (*headersFrame, error) {
	if len(d) < 12 {
		return nil, errParse(d)
	}

	s := &headersFrame{
		Version:  int(fromBig16(d) & 0x7FFF),
		Finished: (d[4] & finishedFlag) != 0,
		StreamId: int(fromBig32(d[8:])),
	}

	if s.StreamId < 0 {
		return s, errStreamProtocol(s.StreamId)
	}

	var err error
	switch s.Version {
	case 2:
		if len(d) < 16 {
			return nil, errStreamProtocol(s.StreamId)
		}
		if s.Header, err = c.Decompress(s.StreamId, s.Version, d[14:]); err != nil {
			return nil, err
		}
	case 3:
		if s.Header, err = c.Decompress(s.StreamId, s.Version, d[12:]); err != nil {
			return nil, err
		}
	default:
		return nil, errStreamVersion{s.StreamId, s.Version}
	}

	return s, nil
}

type rstStreamFrame struct {
	Version  int
	StreamId int
	Reason   int
}

func (s *rstStreamFrame) WriteFrame(w io.Writer, c *compressor) error {
	Log("tx RST_STREAM %+v\n", s)
	h := [16]byte{}
	toBig32(h[0:], rstStreamCode|uint32(s.Version<<16))
	toBig32(h[4:], 8) // length and no flags
	toBig32(h[8:], uint32(s.StreamId))
	toBig32(h[12:], uint32(s.Reason))
	_, err := w.Write(h[:])
	return err
}

func parseRstStream(d []byte) (*rstStreamFrame, error) {
	if len(d) < 12 {
		return nil, errParse(d)
	}

	sid := int(fromBig32(d[8:]))

	if len(d) < 16 || sid < 0 {
		return nil, errStreamProtocol(sid)
	}

	s := &rstStreamFrame{
		Version:  int(fromBig16(d) & 0x7FFF),
		StreamId: sid,
		Reason:   int(fromBig32(d[12:])),
	}

	if s.Reason == 0 {
		return nil, errStreamProtocol(sid)
	}

	return s, nil
}

type windowUpdateFrame struct {
	Version     int
	StreamId    int
	WindowDelta int
}

func (s *windowUpdateFrame) WriteFrame(w io.Writer, c *compressor) error {
	Log("tx WINDOW_UPDATE %+v\n", s)
	h := [16]byte{}
	toBig32(h[0:], windowUpdateCode|uint32(s.Version<<16))
	toBig32(h[4:], 8) // length and no flags
	toBig32(h[8:], uint32(s.StreamId))
	toBig32(h[12:], uint32(s.WindowDelta))
	_, err := w.Write(h[:])
	return err
}

func parseWindowUpdate(d []byte) (*windowUpdateFrame, error) {
	if len(d) < 12 {
		return nil, errParse(d)
	}

	sid := int(fromBig32(d[8:]))

	if len(d) < 16 || sid < 0 {
		return nil, errStreamProtocol(sid)
	}

	s := &windowUpdateFrame{
		Version:     int(fromBig16(d) & 0x7FFF),
		StreamId:    sid,
		WindowDelta: int(fromBig32(d[12:])),
	}

	if s.WindowDelta <= 0 {
		return nil, errStreamProtocol(sid)
	}

	return s, nil
}

type settingsFrame struct {
	Version    int
	HaveWindow bool
	Window     int
}

func (s *settingsFrame) WriteFrame(w io.Writer, c *compressor) error {
	if !s.HaveWindow {
		return nil
	}
	Log("tx SETTINGS %+v\n", s)

	h := [20]byte{}
	toBig32(h[0:], settingsCode|uint32(s.Version<<16))
	toBig32(h[4:], 12) // length from here and no flags
	toBig32(h[8:], 1)  // number of entries

	switch s.Version {
	case 2:
		// V2 has the window given in number of packets, but doesn't say how
		// big each packet is (we are going to use 1KB). It also has the key
		// in little endian.
		toLittle32(h[12:], windowSetting)
		toBig32(h[16:], uint32(s.Window/1024))
	case 3:
		toBig32(h[12:], windowSetting)
		toBig32(h[16:], uint32(s.Window))
	default:
		return errSessionVersion(s.Version)
	}

	_, err := w.Write(h[:])
	return err
}

func parseSettings(d []byte) (*settingsFrame, error) {
	if len(d) < 12 {
		return nil, errParse(d)
	}

	s := &settingsFrame{
		Version: int(fromBig16(d) & 0x7FFF),
	}

	entries := int(fromBig32(d[8:]))
	// limit the number of entries to some max boundary to prevent a
	// overflow when we calc the number of total bytes
	if entries < 0 || entries > 4096 {
		return nil, errParse(d)
	}

	if len(d)-12 < entries*8 {
		return nil, errParse(d)
	}

	d = d[12:]
	for len(d) > 0 {
		var key, val int //, flags int

		switch s.Version {
		case 2:
			//flags = int(d[3])
			key = int(fromLittle32(d[0:]) & 0xFFFFFF)
			val = int(fromBig32(d[4:]))
		case 3:
			//flags = int(d[0])
			key = int(fromBig32(d[0:]) & 0xFFFFFF)
			val = int(fromBig32(d[4:]))
		default:
			return nil, errSessionVersion(s.Version)
		}

		d = d[8:]

		if key == windowSetting {
			s.HaveWindow = true
			s.Window = val
			if s.Version == 2 {
				s.Window *= 1024
			}
			if s.Window < 0 {
				return nil, errSessionProtocol
			}
		}
	}

	return s, nil
}

type pingFrame struct {
	Version int
	Id      uint32
}

func (s *pingFrame) WriteFrame(w io.Writer, c *compressor) error {
	Log("tx PING %+v\n", s)
	h := [12]byte{}
	toBig32(h[0:], pingCode|uint32(s.Version<<16))
	toBig32(h[4:], 4) // length 4 and no flags
	toBig32(h[8:], s.Id)
	_, err := w.Write(h[:])
	return err
}

func parsePing(d []byte) (*pingFrame, error) {
	if len(d) < 12 {
		return nil, errParse(d)
	}

	return &pingFrame{
		Version: int(fromBig16(d) & 0x7FFF),
		Id:      fromBig32(d[8:]),
	}, nil
}

type goAwayFrame struct {
	Version      int
	LastStreamId int
	Reason       int
}

func (s *goAwayFrame) WriteFrame(w io.Writer, c *compressor) (err error) {
	Log("tx GO_AWAY %+v\n", s)
	h := [16]byte{}
	toBig32(h[0:], goAwayCode|uint32(s.Version<<16))
	toBig32(h[4:], 8) // length 8 and no flags
	toBig32(h[8:], uint32(s.LastStreamId))
	toBig32(h[12:], uint32(s.Reason))

	switch s.Version {
	case 2:
		// no reason
		_, err = w.Write(h[:12])
	case 3:
		_, err = w.Write(h[:])
	default:
		err = errSessionVersion(s.Version)
	}

	return err
}

func parseGoAway(d []byte) (*goAwayFrame, error) {
	if len(d) < 12 {
		return nil, errParse(d)
	}

	s := &goAwayFrame{
		Version:      int(fromBig16(d) & 0x7FFF),
		LastStreamId: int(fromBig32(d[8:])),
		Reason:       rstSuccess,
	}

	if s.Version >= 3 {
		if len(d) < 16 {
			return nil, errParse(d)
		}

		s.Reason = int(fromBig32(d[12:]))
	}

	if s.LastStreamId < 0 {
		return nil, errSessionProtocol
	}

	return s, nil
}

type dataFrame struct {
	StreamId int
	Finished bool
	Data     []byte
}

func (s *dataFrame) WriteFrame(w io.Writer, c *compressor) error {
	Log("tx DATA &{StreamId:%d Finished:%v Data:len %d}\n", s.StreamId, s.Finished, len(s.Data))

	flags := uint32(0)
	if s.Finished {
		flags |= finishedFlag << 24
	}

	h := [8]byte{}
	toBig32(h[0:], uint32(s.StreamId))
	toBig32(h[4:], flags|uint32(len(s.Data)))

	if _, err := w.Write(h[:]); err != nil {
		return err
	}

	if _, err := w.Write(s.Data); err != nil {
		return err
	}

	return nil
}

func parseData(d []byte) (*dataFrame, error) {
	s := &dataFrame{
		StreamId: int(fromBig32(d[0:])),
		Finished: (d[4] & finishedFlag) != 0,
		Data:     d[8:],
	}

	if s.StreamId < 0 {
		return nil, errStreamProtocol(s.StreamId)
	}

	return s, nil
}
