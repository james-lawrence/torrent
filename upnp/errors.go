package upnp

import "fmt"

func fromEnvelope(e soapErrorResponse) ErrUPnP {
	return ErrUPnP{
		Code:        e.ErrorCode,
		Description: e.ErrorDescription,
	}
}

type ErrUPnP struct {
	Code        int
	Description string
}

func (t ErrUPnP) Error() string {
	return fmt.Sprintf("UPnP Error: %s (%d)", t.Description, t.Code)
}

func (t ErrUPnP) Is(target error) bool {
	t2, ok := target.(ErrUPnP)
	if !ok {
		return false
	}
	return t.Code == t2.Code && t.Description == t2.Description
}

func (t ErrUPnP) As(target any) bool {
	val, ok := target.(*ErrUPnP)
	if !ok {
		return false
	}
	*val = t
	return true
}
