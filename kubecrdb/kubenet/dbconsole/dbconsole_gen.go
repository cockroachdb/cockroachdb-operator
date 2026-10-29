// This file is manually copied from the CRDB repo. Getting the protos to compile
// requires too much of CRDB's code so we're just going to copy them for now...
// https://github.com/cockroachdb/cockroach/blob/aebcb798d9dce09563dacc56f327e77732290f58/pkg/server/serverpb/authentication.pb.go
package dbconsole

import (
	fmt "fmt"
	io "io"
	math "math"

	proto "github.com/gogo/protobuf/proto" //nolint:importcheck
)

// Reference imports to suppress errors if they are not otherwise used.
var _ = proto.Marshal
var _ = fmt.Errorf
var _ = math.Inf

// This is a compile-time assertion to ensure that this generated file
// is compatible with the proto package it is being compiled against.
// A compilation error at this line likely means your copy of the
// proto package needs to be updated.
const _ = proto.GoGoProtoPackageIsVersion2 // please upgrade the proto package

// SessionCookie is a message used to encode the authentication cookie returned
// from successful login requests.
type SessionCookie struct {
	// The unique ID of the session.
	ID int64 `protobuf:"varint,1,opt,name=id,proto3" json:"id,omitempty"`
	// The secret needed to verify ownership of a session.
	Secret []byte `protobuf:"bytes,2,opt,name=secret,proto3" json:"secret,omitempty"`
}

func (m *SessionCookie) Reset()         { *m = SessionCookie{} }
func (m *SessionCookie) String() string { return proto.CompactTextString(m) }
func (*SessionCookie) ProtoMessage()    {}
func (*SessionCookie) Descriptor() ([]byte, []int) {
	return nil, nil
	// return fileDescriptor_authentication_38fb587d3f7ced6d, []int{4}
}
func (m *SessionCookie) XXX_Unmarshal(b []byte) error {
	return m.Unmarshal(b)
}
func (m *SessionCookie) XXX_Marshal(b []byte, deterministic bool) ([]byte, error) {
	b = b[:cap(b)]
	n, err := m.MarshalTo(b)
	if err != nil {
		return nil, err
	}
	return b[:n], nil
}
func (dst *SessionCookie) XXX_Merge(src proto.Message) {
	xxx_messageInfo_SessionCookie.Merge(dst, src)
}
func (m *SessionCookie) XXX_Size() int {
	return m.Size()
}
func (m *SessionCookie) XXX_DiscardUnknown() {
	xxx_messageInfo_SessionCookie.DiscardUnknown(m)
}

var xxx_messageInfo_SessionCookie proto.InternalMessageInfo

func init() {
	proto.RegisterType((*SessionCookie)(nil), "cockroach.server.serverpb.SessionCookie")
}

func (m *SessionCookie) Size() (n int) {
	if m == nil {
		return 0
	}
	var l int
	_ = l
	if m.ID != 0 {
		n += 1 + sovAuthentication(uint64(m.ID))
	}
	l = len(m.Secret)
	if l > 0 {
		n += 1 + l + sovAuthentication(uint64(l))
	}
	return n
}

func sovAuthentication(x uint64) (n int) {
	for {
		n++
		x >>= 7
		if x == 0 {
			break
		}
	}
	return n
}

//nolint:deadcode,unused
func sozAuthentication(x uint64) (n int) {
	return sovAuthentication(((x << 1) ^ uint64((int64(x) >> 63))))
}

func (m *SessionCookie) Marshal() (dAtA []byte, err error) {
	size := m.Size()
	dAtA = make([]byte, size)
	n, err := m.MarshalTo(dAtA)
	if err != nil {
		return nil, err
	}
	return dAtA[:n], nil
}

func (m *SessionCookie) MarshalTo(dAtA []byte) (int, error) {
	var i int
	_ = i
	var l int
	_ = l
	if m.ID != 0 {
		dAtA[i] = 0x8
		i++
		i = encodeVarintAuthentication(dAtA, i, uint64(m.ID))
	}
	if len(m.Secret) > 0 {
		dAtA[i] = 0x12
		i++
		i = encodeVarintAuthentication(dAtA, i, uint64(len(m.Secret)))
		i += copy(dAtA[i:], m.Secret)
	}
	return i, nil
}

func encodeVarintAuthentication(dAtA []byte, offset int, v uint64) int {
	for v >= 1<<7 {
		dAtA[offset] = uint8(v&0x7f | 0x80)
		v >>= 7
		offset++
	}
	dAtA[offset] = uint8(v)
	return offset + 1
}

func (m *SessionCookie) Unmarshal(dAtA []byte) error {
	l := len(dAtA)
	iNdEx := 0
	for iNdEx < l {
		preIndex := iNdEx
		var wire uint64
		for shift := uint(0); ; shift += 7 {
			if shift >= 64 {
				return ErrIntOverflowAuthentication
			}
			if iNdEx >= l {
				return io.ErrUnexpectedEOF
			}
			b := dAtA[iNdEx]
			iNdEx++
			wire |= (uint64(b) & 0x7F) << shift
			if b < 0x80 {
				break
			}
		}
		fieldNum := int32(wire >> 3)
		wireType := int(wire & 0x7)
		if wireType == 4 {
			return fmt.Errorf("proto: SessionCookie: wiretype end group for non-group")
		}
		if fieldNum <= 0 {
			return fmt.Errorf("proto: SessionCookie: illegal tag %d (wire type %d)", fieldNum, wire)
		}
		switch fieldNum {
		case 1:
			if wireType != 0 {
				return fmt.Errorf("proto: wrong wireType = %d for field ID", wireType)
			}
			m.ID = 0
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowAuthentication
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				m.ID |= (int64(b) & 0x7F) << shift
				if b < 0x80 {
					break
				}
			}
		case 2:
			if wireType != 2 {
				return fmt.Errorf("proto: wrong wireType = %d for field Secret", wireType)
			}
			var byteLen int
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowAuthentication
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				byteLen |= (int(b) & 0x7F) << shift
				if b < 0x80 {
					break
				}
			}
			if byteLen < 0 {
				return ErrInvalidLengthAuthentication
			}
			postIndex := iNdEx + byteLen
			if postIndex > l {
				return io.ErrUnexpectedEOF
			}
			m.Secret = append(m.Secret[:0], dAtA[iNdEx:postIndex]...)
			if m.Secret == nil {
				m.Secret = []byte{}
			}
			iNdEx = postIndex
		default:
			iNdEx = preIndex
			skippy, err := skipAuthentication(dAtA[iNdEx:])
			if err != nil {
				return err
			}
			if skippy < 0 {
				return ErrInvalidLengthAuthentication
			}
			if (iNdEx + skippy) > l {
				return io.ErrUnexpectedEOF
			}
			iNdEx += skippy
		}
	}

	if iNdEx > l {
		return io.ErrUnexpectedEOF
	}
	return nil
}
func skipAuthentication(dAtA []byte) (n int, err error) {
	l := len(dAtA)
	iNdEx := 0
	for iNdEx < l {
		var wire uint64
		for shift := uint(0); ; shift += 7 {
			if shift >= 64 {
				return 0, ErrIntOverflowAuthentication
			}
			if iNdEx >= l {
				return 0, io.ErrUnexpectedEOF
			}
			b := dAtA[iNdEx]
			iNdEx++
			wire |= (uint64(b) & 0x7F) << shift
			if b < 0x80 {
				break
			}
		}
		wireType := int(wire & 0x7)
		switch wireType {
		case 0:
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return 0, ErrIntOverflowAuthentication
				}
				if iNdEx >= l {
					return 0, io.ErrUnexpectedEOF
				}
				iNdEx++
				if dAtA[iNdEx-1] < 0x80 {
					break
				}
			}
			return iNdEx, nil
		case 1:
			iNdEx += 8
			return iNdEx, nil
		case 2:
			var length int
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return 0, ErrIntOverflowAuthentication
				}
				if iNdEx >= l {
					return 0, io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				length |= (int(b) & 0x7F) << shift
				if b < 0x80 {
					break
				}
			}
			iNdEx += length
			if length < 0 {
				return 0, ErrInvalidLengthAuthentication
			}
			return iNdEx, nil
		case 3:
			for {
				var innerWire uint64
				var start int = iNdEx
				for shift := uint(0); ; shift += 7 {
					if shift >= 64 {
						return 0, ErrIntOverflowAuthentication
					}
					if iNdEx >= l {
						return 0, io.ErrUnexpectedEOF
					}
					b := dAtA[iNdEx]
					iNdEx++
					innerWire |= (uint64(b) & 0x7F) << shift
					if b < 0x80 {
						break
					}
				}
				innerWireType := int(innerWire & 0x7)
				if innerWireType == 4 {
					break
				}
				next, err := skipAuthentication(dAtA[start:])
				if err != nil {
					return 0, err
				}
				iNdEx = start + next
			}
			return iNdEx, nil
		case 4:
			return iNdEx, nil
		case 5:
			iNdEx += 4
			return iNdEx, nil
		default:
			return 0, fmt.Errorf("proto: illegal wireType %d", wireType)
		}
	}
	panic("unreachable")
}

var (
	ErrInvalidLengthAuthentication = fmt.Errorf("proto: negative length found during unmarshaling")
	ErrIntOverflowAuthentication   = fmt.Errorf("proto: integer overflow")
)
