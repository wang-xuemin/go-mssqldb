package mssql

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	alwaysencrypted "github.com/swisscom/mssql-always-encrypted/pkg"
	"github.com/swisscom/mssql-always-encrypted/pkg/algorithms"
	"github.com/swisscom/mssql-always-encrypted/pkg/encryption"
	"github.com/swisscom/mssql-always-encrypted/pkg/keys"
	"golang.org/x/crypto/pkcs12"
	"golang.org/x/text/encoding/unicode"
	"io"
	"io/ioutil"
	"os"
	"strconv"
)

//go:generate go run golang.org/x/tools/cmd/stringer -type token

type token byte

// token ids
const (
	tokenReturnStatus  token = 121 // 0x79
	tokenColMetadata   token = 129 // 0x81
	tokenOrder         token = 169 // 0xA9
	tokenError         token = 170 // 0xAA
	tokenInfo          token = 171 // 0xAB
	tokenReturnValue   token = 0xAC
	tokenLoginAck      token = 173 // 0xad
	tokenFeatureExtAck token = 174 // 0xae
	tokenRow           token = 209 // 0xd1
	tokenNbcRow        token = 210 // 0xd2
	tokenEnvChange     token = 227 // 0xE3
	tokenSSPI          token = 237 // 0xED
	tokenFedAuthInfo   token = 238 // 0xEE
	tokenDone          token = 253 // 0xFD
	tokenDoneProc      token = 254
	tokenDoneInProc    token = 255
)

// done flags
// https://msdn.microsoft.com/en-us/library/dd340421.aspx
const (
	doneFinal    = 0
	doneMore     = 1
	doneError    = 2
	doneInxact   = 4
	doneCount    = 0x10
	doneAttn     = 0x20
	doneSrvError = 0x100
)

// ENVCHANGE types
// http://msdn.microsoft.com/en-us/library/dd303449.aspx
const (
	envTypDatabase           = 1
	envTypLanguage           = 2
	envTypCharset            = 3
	envTypPacketSize         = 4
	envSortId                = 5
	envSortFlags             = 6
	envSqlCollation          = 7
	envTypBeginTran          = 8
	envTypCommitTran         = 9
	envTypRollbackTran       = 10
	envEnlistDTC             = 11
	envDefectTran            = 12
	envDatabaseMirrorPartner = 13
	envPromoteTran           = 15
	envTranMgrAddr           = 16
	envTranEnded             = 17
	envResetConnAck          = 18
	envStartedInstanceName   = 19
	envRouting               = 20
)

const (
	fedAuthInfoSTSURL = 0x01
	fedAuthInfoSPN    = 0x02
)

const (
	cipherAlgCustom = 0x00
)

// COLMETADATA flags
// https://msdn.microsoft.com/en-us/library/dd357363.aspx
const (
	colFlagNullable = 1
	// TODO implement more flags
)

// UTF-16 Decoder
var utf16Decoder = unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()

// interface for all tokens
type tokenStruct interface{}

type orderStruct struct {
	ColIds []uint16
}

type doneStruct struct {
	Status   uint16
	CurCmd   uint16
	RowCount uint64
	errors   []Error
}

func (d doneStruct) isError() bool {
	return d.Status&doneError != 0 || len(d.errors) > 0
}

func (d doneStruct) getError() Error {
	if len(d.errors) > 0 {
		return d.errors[len(d.errors)-1]
	} else {
		return Error{Message: "Request failed but didn't provide reason"}
	}
}

type doneInProcStruct doneStruct

// ENVCHANGE stream
// http://msdn.microsoft.com/en-us/library/dd303449.aspx
func processEnvChg(sess *tdsSession) {
	size := sess.buf.uint16()
	r := &io.LimitedReader{R: sess.buf, N: int64(size)}
	for {
		var err error
		var envtype uint8
		err = binary.Read(r, binary.LittleEndian, &envtype)
		if err == io.EOF {
			return
		}
		if err != nil {
			badStreamPanic(err)
		}
		switch envtype {
		case envTypDatabase:
			sess.database, err = readBVarChar(r)
			if err != nil {
				badStreamPanic(err)
			}
			_, err = readBVarChar(r)
			if err != nil {
				badStreamPanic(err)
			}
		case envTypLanguage:
			// currently ignored
			// new value
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
			// old value
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
		case envTypCharset:
			// currently ignored
			// new value
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
			// old value
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
		case envTypPacketSize:
			packetsize, err := readBVarChar(r)
			if err != nil {
				badStreamPanic(err)
			}
			_, err = readBVarChar(r)
			if err != nil {
				badStreamPanic(err)
			}
			packetsizei, err := strconv.Atoi(packetsize)
			if err != nil {
				badStreamPanicf("Invalid Packet size value returned from server (%s): %s", packetsize, err.Error())
			}
			sess.buf.ResizeBuffer(packetsizei)
		case envSortId:
			// currently ignored
			// new value
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
			// old value, should be 0
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
		case envSortFlags:
			// currently ignored
			// new value
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
			// old value, should be 0
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
		case envSqlCollation:
			// currently ignored
			var collationSize uint8
			err = binary.Read(r, binary.LittleEndian, &collationSize)
			if err != nil {
				badStreamPanic(err)
			}

			// SQL Collation data should contain 5 bytes in length
			if collationSize != 5 {
				badStreamPanicf("Invalid SQL Collation size value returned from server: %d", collationSize)
			}

			// 4 bytes, contains: LCID ColFlags Version
			var info uint32
			err = binary.Read(r, binary.LittleEndian, &info)
			if err != nil {
				badStreamPanic(err)
			}

			// 1 byte, contains: sortID
			var sortID uint8
			err = binary.Read(r, binary.LittleEndian, &sortID)
			if err != nil {
				badStreamPanic(err)
			}

			// old value, should be 0
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
		case envTypBeginTran:
			tranid, err := readBVarByte(r)
			if len(tranid) != 8 {
				badStreamPanicf("invalid size of transaction identifier: %d", len(tranid))
			}
			sess.tranid = binary.LittleEndian.Uint64(tranid)
			if err != nil {
				badStreamPanic(err)
			}
			if sess.logFlags&logTransaction != 0 {
				sess.log.Printf("BEGIN TRANSACTION %x\n", sess.tranid)
			}
			_, err = readBVarByte(r)
			if err != nil {
				badStreamPanic(err)
			}
		case envTypCommitTran, envTypRollbackTran:
			_, err = readBVarByte(r)
			if err != nil {
				badStreamPanic(err)
			}
			_, err = readBVarByte(r)
			if err != nil {
				badStreamPanic(err)
			}
			if sess.logFlags&logTransaction != 0 {
				if envtype == envTypCommitTran {
					sess.log.Printf("COMMIT TRANSACTION %x\n", sess.tranid)
				} else {
					sess.log.Printf("ROLLBACK TRANSACTION %x\n", sess.tranid)
				}
			}
			sess.tranid = 0
		case envEnlistDTC:
			// currently ignored
			// new value, should be 0
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
			// old value
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
		case envDefectTran:
			// currently ignored
			// new value
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
			// old value, should be 0
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
		case envDatabaseMirrorPartner:
			sess.partner, err = readBVarChar(r)
			if err != nil {
				badStreamPanic(err)
			}
			_, err = readBVarChar(r)
			if err != nil {
				badStreamPanic(err)
			}
		case envPromoteTran:
			// currently ignored
			// old value, should be 0
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
			// dtc token
			// spec says it should be L_VARBYTE, so this code might be wrong
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
		case envTranMgrAddr:
			// currently ignored
			// old value, should be 0
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
			// XACT_MANAGER_ADDRESS = B_VARBYTE
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
		case envTranEnded:
			// currently ignored
			// old value, B_VARBYTE
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
			// should be 0
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
		case envResetConnAck:
			// currently ignored
			// old value, should be 0
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
			// should be 0
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
		case envStartedInstanceName:
			// currently ignored
			// old value, should be 0
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
			// instance name
			if _, err = readBVarChar(r); err != nil {
				badStreamPanic(err)
			}
		case envRouting:
			// RoutingData message is:
			// ValueLength                 USHORT
			// Protocol (TCP = 0)          BYTE
			// ProtocolProperty (new port) USHORT
			// AlternateServer             US_VARCHAR
			_, err := readUshort(r)
			if err != nil {
				badStreamPanic(err)
			}
			protocol, err := readByte(r)
			if err != nil || protocol != 0 {
				badStreamPanic(err)
			}
			newPort, err := readUshort(r)
			if err != nil {
				badStreamPanic(err)
			}
			newServer, err := readUsVarChar(r)
			if err != nil {
				badStreamPanic(err)
			}
			// consume the OLDVALUE = %x00 %x00
			_, err = readUshort(r)
			if err != nil {
				badStreamPanic(err)
			}
			sess.routedServer = newServer
			sess.routedPort = newPort
		default:
			// ignore rest of records because we don't know how to skip those
			sess.log.Printf("WARN: Unknown ENVCHANGE record detected with type id = %d\n", envtype)
			return
		}
	}
}

// http://msdn.microsoft.com/en-us/library/dd358180.aspx
func parseReturnStatus(r *tdsBuffer) ReturnStatus {
	return ReturnStatus(r.int32())
}

func parseOrder(r *tdsBuffer) (res orderStruct) {
	len := int(r.uint16())
	res.ColIds = make([]uint16, len/2)
	for i := 0; i < len/2; i++ {
		res.ColIds[i] = r.uint16()
	}
	return res
}

// https://msdn.microsoft.com/en-us/library/dd340421.aspx
func parseDone(r *tdsBuffer) (res doneStruct) {
	res.Status = r.uint16()
	res.CurCmd = r.uint16()
	res.RowCount = r.uint64()
	return res
}

// https://msdn.microsoft.com/en-us/library/dd340553.aspx
func parseDoneInProc(r *tdsBuffer) (res doneInProcStruct) {
	res.Status = r.uint16()
	res.CurCmd = r.uint16()
	res.RowCount = r.uint64()
	return res
}

type sspiMsg []byte

func parseSSPIMsg(r *tdsBuffer) sspiMsg {
	size := r.uint16()
	buf := make([]byte, size)
	r.ReadFull(buf)
	return sspiMsg(buf)
}

type fedAuthInfoStruct struct {
	STSURL    string
	ServerSPN string
}

type fedAuthInfoOpt struct {
	fedAuthInfoID          byte
	dataLength, dataOffset uint32
}

func parseFedAuthInfo(r *tdsBuffer) fedAuthInfoStruct {
	size := r.uint32()

	var STSURL, SPN string
	var err error

	// Each fedAuthInfoOpt is one byte to indicate the info ID,
	// then a four byte offset and a four byte length.
	count := r.uint32()
	offset := uint32(4)
	opts := make([]fedAuthInfoOpt, count)

	for i := uint32(0); i < count; i++ {
		fedAuthInfoID := r.byte()
		dataLength := r.uint32()
		dataOffset := r.uint32()
		offset += 1 + 4 + 4

		opts[i] = fedAuthInfoOpt{
			fedAuthInfoID: fedAuthInfoID,
			dataLength:    dataLength,
			dataOffset:    dataOffset,
		}
	}

	data := make([]byte, size-offset)
	r.ReadFull(data)

	for i := uint32(0); i < count; i++ {
		if opts[i].dataOffset < offset {
			badStreamPanicf("Fed auth info opt stated data offset %d is before data begins in packet at %d",
				opts[i].dataOffset, offset)
			// returns via panic
		}

		if opts[i].dataOffset+opts[i].dataLength > size {
			badStreamPanicf("Fed auth info opt stated data length %d added to stated offset exceeds size of packet %d",
				opts[i].dataOffset+opts[i].dataLength, size)
			// returns via panic
		}

		optData := data[opts[i].dataOffset-offset : opts[i].dataOffset-offset+opts[i].dataLength]
		switch opts[i].fedAuthInfoID {
		case fedAuthInfoSTSURL:
			STSURL, err = ucs22str(optData)
		case fedAuthInfoSPN:
			SPN, err = ucs22str(optData)
		default:
			err = fmt.Errorf("unexpected fed auth info opt ID %d", int(opts[i].fedAuthInfoID))
		}

		if err != nil {
			badStreamPanic(err)
		}
	}

	return fedAuthInfoStruct{
		STSURL:    STSURL,
		ServerSPN: SPN,
	}
}

type loginAckStruct struct {
	Interface  uint8
	TDSVersion uint32
	ProgName   string
	ProgVer    uint32
}

func parseLoginAck(r *tdsBuffer) loginAckStruct {
	size := r.uint16()
	buf := make([]byte, size)
	r.ReadFull(buf)
	var res loginAckStruct
	res.Interface = buf[0]
	res.TDSVersion = binary.BigEndian.Uint32(buf[1:])
	prognamelen := buf[1+4]
	var err error
	if res.ProgName, err = ucs22str(buf[1+4+1 : 1+4+1+prognamelen*2]); err != nil {
		badStreamPanic(err)
	}
	res.ProgVer = binary.BigEndian.Uint32(buf[size-4:])
	return res
}

// https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-tds/2eb82f8e-11f0-46dc-b42d-27302fa4701a
type fedAuthAckStruct struct {
	Nonce     []byte
	Signature []byte
}

type colAckStruct struct {
	Version int
}

type featureExtAck map[byte]interface{}

func parseFeatureExtAck(r *tdsBuffer) featureExtAck {
	ack := map[byte]interface{}{}

	for feature := r.byte(); feature != featExtTERMINATOR; feature = r.byte() {
		length := r.uint32()

		switch feature {
		case featExtFEDAUTH:
			// In theory we need to know the federated authentication library to
			// know how to parse, but the alternatives provide compatible structures.
			fedAuthAck := fedAuthAckStruct{}
			if length >= 32 {
				fedAuthAck.Nonce = make([]byte, 32)
				r.ReadFull(fedAuthAck.Nonce)
				length -= 32
			}
			if length >= 32 {
				fedAuthAck.Signature = make([]byte, 32)
				r.ReadFull(fedAuthAck.Signature)
				length -= 32
			}
			ack[feature] = fedAuthAck
		case featExtCOLUMNENCRYPTION:
			colAck := colAckStruct{}
			colAck.Version = int(r.byte())
			length--

			if length > 0 {
				enclaveLength := r.byte()
				var enclaveType = make([]byte, enclaveLength)
				r.ReadFull(enclaveType)
				length -= uint32(enclaveLength)
			}
			ack[feature] = colAck
		}

		// Skip unprocessed bytes
		if length > 0 {
			io.CopyN(ioutil.Discard, r, int64(length))
		}
	}

	return ack
}

// http://msdn.microsoft.com/en-us/library/dd357363.aspx
func parseColMetadata72(r *tdsBuffer, s *tdsSession) (columns []columnStruct) {
	count := r.uint16()
	if count == 0xffff {
		// no metadata is sent
		return nil
	}
	columns = make([]columnStruct, count)

	var cekTable *cekTable
	if s.alwaysEncrypted {
		// CEK table
		cekTable = readCEKTable(r)

		if s.alwaysEncryptedSettings == nil {
			panic("alwaysEncryptedSettings are nil!")
		}

		if s.alwaysEncryptedSettings.pKey == nil {
			// Load Keystore
			f, err := os.Open(s.alwaysEncryptedSettings.ksLocation)
			if err != nil {
				panic(err)
			}

			switch s.alwaysEncryptedSettings.ksAuth {
			case PFXKeystoreAuth:
				pfxBytes, err := ioutil.ReadAll(f)
				if err != nil {
					panic(err)
				}

				pk, cert, err := pkcs12.Decode(pfxBytes, s.alwaysEncryptedSettings.ksSecret)
				if err != nil {
					panic(err)
				}

				s.alwaysEncryptedSettings.pKey = pk
				s.alwaysEncryptedSettings.cert = cert
			default:
				panic(fmt.Sprintf("ksAuth %v is unimplemented", s.alwaysEncryptedSettings.ksAuth))
			}
		}
	}

	dec := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()

	for i := range columns {
		column := &columns[i]
		baseTi := getBaseTypeInfo(r, true)
		typeInfo := readTypeInfo(r, baseTi.TypeId, column.cryptoMeta)
		typeInfo.UserType = baseTi.UserType
		typeInfo.Flags = baseTi.Flags
		typeInfo.TypeId = baseTi.TypeId

		// Table Name
		if baseTi.TypeId == typeText || baseTi.TypeId == typeNText || baseTi.TypeId == typeImage {
			_ = r.sqlIdentifier()
		}

		column.Flags = baseTi.Flags
		column.UserType = baseTi.UserType
		column.ti = typeInfo

		if column.isEncrypted() && s.alwaysEncrypted {
			// Read Crypto Metadata
			cryptoMeta := parseCryptoMetadata(r, cekTable)
			cryptoMeta.typeInfo.Flags = baseTi.Flags
			column.cryptoMeta = &cryptoMeta
		} else {
			column.cryptoMeta = nil
		}

		colNameLen := r.byte()
		colNameUtf16 := make([]byte, int(colNameLen)*2)
		r.ReadFull(colNameUtf16)
		colName, _ := dec.Bytes(colNameUtf16)
		column.ColName = string(colName)
	}
	return columns
}

func getBaseTypeInfo(r *tdsBuffer, parseFlags bool) typeInfo {
	userType := r.uint32()
	flags := uint16(0)
	if parseFlags {
		flags = r.uint16()
	}
	tId := r.byte()

	return typeInfo{
		UserType: userType,
		Flags:    flags,
		TypeId:   tId}
}

type cryptoMetadata struct {
	entry         *cekTableEntry
	ordinal       uint16
	algorithmId   byte
	algorithmName *string
	encType       byte
	normRuleVer   byte
	typeInfo      typeInfo
}

func parseCryptoMetadata(r *tdsBuffer, cekTable *cekTable) cryptoMetadata {
	ordinal := uint16(0)
	if cekTable != nil {
		ordinal = r.uint16()
	}

	typeInfo := getBaseTypeInfo(r, false)
	ti := readTypeInfo(r, typeInfo.TypeId, nil)
	ti.UserType = typeInfo.UserType
	ti.Flags = typeInfo.Flags
	ti.TypeId = typeInfo.TypeId

	algorithmId := r.byte()
	var algName *string = nil

	if algorithmId == cipherAlgCustom {
		// Read the name when a custom algorithm is used
		nameLen := int(r.byte())
		var algNameUtf16 = make([]byte, nameLen*2)
		r.ReadFull(algNameUtf16)
		algNameBytes, _ := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder().Bytes(algNameUtf16)
		mAlgName := string(algNameBytes)
		algName = &mAlgName
	}

	encType := r.byte()
	normRuleVer := r.byte()

	var entry *cekTableEntry = nil

	if cekTable != nil {
		if int(ordinal) > len(cekTable.entries)-1 {
			panic(fmt.Errorf("invalid ordinal, cekTable only has %d entries", len(cekTable.entries)))
		}
		entry = &cekTable.entries[ordinal]
	}

	return cryptoMetadata{
		entry:         entry,
		ordinal:       ordinal,
		algorithmId:   algorithmId,
		algorithmName: algName,
		encType:       encType,
		normRuleVer:   normRuleVer,
		typeInfo:      ti,
	}
}

func readCEKTable(r *tdsBuffer) *cekTable {
	tableSize := r.uint16()
	var cekTable *cekTable = nil

	if tableSize != 0 {
		mCekTable := newCekTable(tableSize)
		for i := uint16(0); i < tableSize; i++ {
			mCekTable.entries[i] = readCekTableEntry(r)
		}
		cekTable = &mCekTable
	}

	return cekTable
}

func readCekTableEntry(r *tdsBuffer) cekTableEntry {
	databaseId := r.int32()
	cekID := r.int32()
	cekVersion := r.int32()
	var cekMdVersion = make([]byte, 8)
	_, err := r.Read(cekMdVersion)
	if err != nil {
		panic("unable to read cekMdVersion")
	}

	cekValueCount := uint(r.byte())
	enc := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM)
	utf16dec := enc.NewDecoder()
	cekValues := make([]encryptionKeyInfo, cekValueCount)

	for i := uint(0); i < cekValueCount; i++ {
		encryptedCekLength := r.uint16()
		encryptedCek := make([]byte, encryptedCekLength)
		r.ReadFull(encryptedCek)

		keyStoreLength := r.byte()
		keyStoreNameUtf16 := make([]byte, keyStoreLength*2)
		r.ReadFull(keyStoreNameUtf16)
		keyStoreName, _ := utf16dec.Bytes(keyStoreNameUtf16)

		keyPathLength := r.uint16()
		keyPathUtf16 := make([]byte, keyPathLength*2)
		r.ReadFull(keyPathUtf16)
		keyPath, _ := utf16dec.Bytes(keyPathUtf16)

		algLength := r.byte()
		algNameUtf16 := make([]byte, algLength*2)
		r.ReadFull(algNameUtf16)
		algName, _ := utf16dec.Bytes(algNameUtf16)

		cekValues[i] = encryptionKeyInfo{
			encryptedKey:  encryptedCek,
			databaseID:    int(databaseId),
			cekID:         int(cekID),
			cekVersion:    int(cekVersion),
			cekMdVersion:  cekMdVersion,
			keyPath:       string(keyPath),
			keyStoreName:  string(keyStoreName),
			algorithmName: string(algName),
		}
	}

	return cekTableEntry{
		databaseID: int(databaseId),
		keyId:      int(cekID),
		keyVersion: int(cekVersion),
		mdVersion:  cekMdVersion,
		valueCount: int(cekValueCount),
		cekValues:  cekValues,
	}
}

type RWCBuffer struct {
	buffer *bytes.Reader
}

func (R RWCBuffer) Read(p []byte) (n int, err error) {
	return R.buffer.Read(p)
}

func (R RWCBuffer) Write(p []byte) (n int, err error) {
	return 0, nil
}

func (R RWCBuffer) Close() error {
	return nil
}

var _ io.ReadWriteCloser = RWCBuffer{}

// http://msdn.microsoft.com/en-us/library/dd357254.aspx
func parseRow(r *tdsBuffer, s *tdsSession, columns []columnStruct, row []interface{}) {
	for i, column := range columns {
		columnContent := column.ti.Reader(&column.ti, r, nil)
		if columnContent == nil {
			row[i] = columnContent
			continue
		}

		if column.isEncrypted() && s.alwaysEncrypted {
			buffer := decryptColumn(column, s, columnContent)
			// Decrypt
			row[i] = column.cryptoMeta.typeInfo.Reader(&column.cryptoMeta.typeInfo, &buffer, column.cryptoMeta)
		} else {
			row[i] = columnContent
		}
	}
}

func decryptColumn(column columnStruct, s *tdsSession, columnContent interface{}) tdsBuffer {
	// Decrypt
	cekValue := column.cryptoMeta.entry.cekValues[column.cryptoMeta.ordinal]
	algVer := cekValue.cekVersion
	encType := encryption.From(column.cryptoMeta.encType)

	// Get pKey
	if s.alwaysEncryptedSettings.pKey == nil {
		panic("alwaysEncrypted pKey not set: this should never happen")
	}

	cekv := alwaysencrypted.LoadCEKV(column.cryptoMeta.entry.cekValues[0].encryptedKey)
	if !cekv.Verify(s.alwaysEncryptedSettings.cert) {
		panic(fmt.Errorf("invalid certificate being used to decrypt: %v requested but %v provided",
			cekv.KeyPath,
			fmt.Sprintf("%02x", sha1.Sum(s.alwaysEncryptedSettings.cert.Raw)),
		))
	}

	// TODO: Support other private keys
	rootKey, err := cekv.Decrypt(s.alwaysEncryptedSettings.pKey.(*rsa.PrivateKey))
	if err != nil {
		panic(err)
	}

	// Derive Root Key from encryptedKey
	k := keys.NewAeadAes256CbcHmac256(rootKey)
	alg := algorithms.NewAeadAes256CbcHmac256Algorithm(k, encType, byte(algVer))

	d, err := alg.Decrypt(columnContent.([]byte))
	if err != nil {
		panic(err)
	}

	// Dirty workaround to keep compatibility with original types
	// TODO: Improve me
	var newBuff []byte
	newBuff = append(newBuff, d...)

	rwc := RWCBuffer{
		buffer: bytes.NewReader(newBuff),
	}

	column.cryptoMeta.typeInfo.Buffer = d
	buffer := tdsBuffer{rpos: 0, rsize: len(newBuff), rbuf: newBuff, transport: rwc}
	return buffer
}

// http://msdn.microsoft.com/en-us/library/dd304783.aspx
func parseNbcRow(r *tdsBuffer, s *tdsSession, columns []columnStruct, row []interface{}) {
	bitlen := (len(columns) + 7) / 8
	pres := make([]byte, bitlen)
	r.ReadFull(pres)
	for i, col := range columns {
		if pres[i/8]&(1<<(uint(i)%8)) != 0 {
			row[i] = nil
			continue
		}
		columnContent := col.ti.Reader(&col.ti, r, nil)
		if col.isEncrypted() && s.alwaysEncrypted {
			buffer := decryptColumn(col, s, columnContent)
			// Decrypt
			row[i] = col.cryptoMeta.typeInfo.Reader(&col.cryptoMeta.typeInfo, &buffer, col.cryptoMeta)
		} else {
			row[i] = columnContent
		}
	}
}

// http://msdn.microsoft.com/en-us/library/dd304156.aspx
func parseError72(r *tdsBuffer) (res Error) {
	length := r.uint16()
	_ = length // ignore length
	res.Number = r.int32()
	res.State = r.byte()
	res.Class = r.byte()
	res.Message = r.UsVarChar()
	res.ServerName = r.BVarChar()
	res.ProcName = r.BVarChar()
	res.LineNo = r.int32()
	return
}

// http://msdn.microsoft.com/en-us/library/dd304156.aspx
func parseInfo(r *tdsBuffer) (res Error) {
	length := r.uint16()
	_ = length // ignore length
	res.Number = r.int32()
	res.State = r.byte()
	res.Class = r.byte()
	res.Message = r.UsVarChar()
	res.ServerName = r.BVarChar()
	res.ProcName = r.BVarChar()
	res.LineNo = r.int32()
	return
}

// https://msdn.microsoft.com/en-us/library/dd303881.aspx
func parseReturnValue(r *tdsBuffer, s *tdsSession) (nv namedValue) {
	/*
		ParamOrdinal
		ParamName
		Status
		UserType
		Flags
		TypeInfo
		CryptoMetadata
		Value
	*/
	_ = r.uint16() // ParamOrdinal
	nv.Name = r.BVarChar() // ParamName
	_ = r.byte() // Status

	ti := getBaseTypeInfo(r, true) // UserType + Flags + TypeInfo

	var cryptoMetadata *cryptoMetadata = nil
	if s.alwaysEncrypted {
		cm := parseCryptoMetadata(r, nil) // CryptoMetadata
		cryptoMetadata = &cm
	}

	ti2 := readTypeInfo(r, ti.TypeId, cryptoMetadata)
	nv.Value = ti2.Reader(&ti2, r, cryptoMetadata)

	return
}

func processSingleResponse(sess *tdsSession, ch chan tokenStruct, outs map[string]interface{}) {
	defer func() {
		if err := recover(); err != nil {
			if sess.logFlags&logErrors != 0 {
				sess.log.Printf("ERROR: Intercepted panic %v", err)
			}
			ch <- err
		}
		close(ch)
	}()

	packet_type, err := sess.buf.BeginRead()
	if err != nil {
		if sess.logFlags&logErrors != 0 {
			sess.log.Printf("ERROR: BeginRead failed %v", err)
		}
		ch <- err
		return
	}
	if packet_type != packReply {
		badStreamPanic(fmt.Errorf("unexpected packet type in reply: got %v, expected %v", packet_type, packReply))
	}
	var columns []columnStruct
	errs := make([]Error, 0, 5)
	for tokens := 0; ; tokens += 1 {
		token := token(sess.buf.byte())
		if sess.logFlags&logDebug != 0 {
			sess.log.Printf("got token %v", token)
		}
		switch token {
		case tokenSSPI:
			ch <- parseSSPIMsg(sess.buf)
			return
		case tokenFedAuthInfo:
			ch <- parseFedAuthInfo(sess.buf)
			return
		case tokenReturnStatus:
			returnStatus := parseReturnStatus(sess.buf)
			ch <- returnStatus
		case tokenLoginAck:
			loginAck := parseLoginAck(sess.buf)
			ch <- loginAck
		case tokenFeatureExtAck:
			featureExtAck := parseFeatureExtAck(sess.buf)
			ch <- featureExtAck
		case tokenOrder:
			order := parseOrder(sess.buf)
			ch <- order
		case tokenDoneInProc:
			done := parseDoneInProc(sess.buf)
			if sess.logFlags&logRows != 0 && done.Status&doneCount != 0 {
				sess.log.Printf("(%d row(s) affected)\n", done.RowCount)
			}
			ch <- done
		case tokenDone, tokenDoneProc:
			done := parseDone(sess.buf)
			done.errors = errs
			if sess.logFlags&logDebug != 0 {
				sess.log.Printf("got DONE or DONEPROC status=%d", done.Status)
			}
			if done.Status&doneSrvError != 0 {
				ch <- errors.New("SQL Server had internal error")
				return
			}
			if sess.logFlags&logRows != 0 && done.Status&doneCount != 0 {
				sess.log.Printf("(%d row(s) affected)\n", done.RowCount)
			}
			ch <- done
			if done.Status&doneMore == 0 {
				return
			}
		case tokenColMetadata:
			columns = parseColMetadata72(sess.buf, sess)
			ch <- columns
		case tokenRow:
			row := make([]interface{}, len(columns))
			parseRow(sess.buf, sess, columns, row)
			ch <- row
		case tokenNbcRow:
			row := make([]interface{}, len(columns))
			parseNbcRow(sess.buf, sess, columns, row)
			ch <- row
		case tokenEnvChange:
			processEnvChg(sess)
		case tokenError:
			err := parseError72(sess.buf)
			if sess.logFlags&logDebug != 0 {
				sess.log.Printf("got ERROR %d %s", err.Number, err.Message)
			}
			errs = append(errs, err)
			if sess.logFlags&logErrors != 0 {
				sess.log.Println(err.Message)
			}
		case tokenInfo:
			info := parseInfo(sess.buf)
			if sess.logFlags&logDebug != 0 {
				sess.log.Printf("got INFO %d %s", info.Number, info.Message)
			}
			if sess.logFlags&logMessages != 0 {
				sess.log.Println(info.Message)
			}
		case tokenReturnValue:
			nv := parseReturnValue(sess.buf, sess)
			if len(nv.Name) > 0 {
				name := nv.Name[1:] // Remove the leading "@".
				if ov, has := outs[name]; has {
					err = scanIntoOut(name, nv.Value, ov)
					if err != nil {
						fmt.Println("scan error", err)
						ch <- err
					}
				}
			}
		default:
			badStreamPanic(fmt.Errorf("unknown token type returned: %v", token))
		}
	}
}

type tokenProcessor struct {
	tokChan    chan tokenStruct
	ctx        context.Context
	sess       *tdsSession
	outs       map[string]interface{}
	lastRow    []interface{}
	rowCount   int64
	firstError error
}

func startReading(sess *tdsSession, ctx context.Context, outs map[string]interface{}) *tokenProcessor {
	tokChan := make(chan tokenStruct, 5)
	go processSingleResponse(sess, tokChan, outs)
	return &tokenProcessor{
		tokChan: tokChan,
		ctx:     ctx,
		sess:    sess,
		outs:    outs,
	}
}

func (t *tokenProcessor) iterateResponse() error {
	for {
		tok, err := t.nextToken()
		if err == nil {
			if tok == nil {
				return t.firstError
			} else {
				switch token := tok.(type) {
				case []columnStruct:
					t.sess.columns = token
				case []interface{}:
					t.lastRow = token
				case doneInProcStruct:
					if token.Status&doneCount != 0 {
						t.rowCount += int64(token.RowCount)
					}
				case doneStruct:
					if token.Status&doneCount != 0 {
						t.rowCount += int64(token.RowCount)
					}
					if token.isError() && t.firstError == nil {
						t.firstError = token.getError()
					}
				case ReturnStatus:
					t.sess.setReturnStatus(token)
					/*case error:
					if resultError == nil {
						resultError = token
					}*/
				}
			}
		} else {
			return err
		}
	}
}

func (t tokenProcessor) nextToken() (tokenStruct, error) {
	// we do this separate non-blocking check on token channel to
	// prioritize it over cancellation channel
	select {
	case tok, more := <-t.tokChan:
		err, more := tok.(error)
		if more {
			// this is an error and not a token
			return nil, err
		} else {
			return tok, nil
		}
	default:
		// there are no tokens on the channel, will need to wait
	}

	select {
	case tok, more := <-t.tokChan:
		if more {
			err, ok := tok.(error)
			if ok {
				// this is an error and not a token
				return nil, err
			} else {
				return tok, nil
			}
		} else {
			// completed reading response
			return nil, nil
		}
	case <-t.ctx.Done():
		if err := sendAttention(t.sess.buf); err != nil {
			// unable to send attention, current connection is bad
			// notify caller and close channel
			return nil, err
		}

		// now the server should send cancellation confirmation
		// it is possible that we already received full response
		// just before we sent cancellation request
		// in this case current response would not contain confirmation
		// and we would need to read one more response

		// first lets finish reading current response and look
		// for confirmation in it
		if readCancelConfirmation(t.tokChan) {
			// we got confirmation in current response
			return nil, t.ctx.Err()
		}
		// we did not get cancellation confirmation in the current response
		// read one more response, it must be there
		t.tokChan = make(chan tokenStruct, 5)
		go processSingleResponse(t.sess, t.tokChan, t.outs)
		if readCancelConfirmation(t.tokChan) {
			return nil, t.ctx.Err()
		}
		// we did not get cancellation confirmation, something is not
		// right, this connection is not usable anymore
		return nil, errors.New("did not get cancellation confirmation from the server")
	}
}

func readCancelConfirmation(tokChan chan tokenStruct) bool {
	for tok := range tokChan {
		switch tok := tok.(type) {
		default:
		// just skip token
		case doneStruct:
			if tok.Status&doneAttn != 0 {
				// got cancellation confirmation, exit
				return true
			}
		}
	}
	return false
}