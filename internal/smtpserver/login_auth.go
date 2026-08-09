package smtpserver

import "github.com/emersion/go-sasl"

type loginAuthFunc func(username, password string) error

type loginServer struct {
	auth loginAuthFunc
	step int
	user string
}

func newLoginServer(auth loginAuthFunc) sasl.Server {
	return &loginServer{auth: auth}
}

func (s *loginServer) Next(response []byte) (challenge []byte, done bool, err error) {
	switch s.step {
	case 0:
		s.step++
		if response != nil {
			s.user = string(response)
			return []byte("Password:"), false, nil
		}
		return []byte("Username:"), false, nil
	case 1:
		s.step++
		if s.user == "" {
			s.user = string(response)
			return []byte("Password:"), false, nil
		}
		if err := s.auth(s.user, string(response)); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	case 2:
		s.step++
		if err := s.auth(s.user, string(response)); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	default:
		return nil, false, sasl.ErrUnexpectedClientResponse
	}
}
