package quic

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lucas-clemente/quic-go"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

var (
	testSessionID = uuid.New()
)

func TestSuffixThenRemoveSessionID(t *testing.T) {
	msg := []byte(t.Name())
	msgWithID, err := suffixSessionID(testSessionID, msg)
	require.NoError(t, err)
	require.Len(t, msgWithID, len(msg)+sessionIDLen)

	sessionID, msgWithoutID, err := extractSessionID(msgWithID)
	require.NoError(t, err)
	require.Equal(t, msg, msgWithoutID)
	require.Equal(t, testSessionID, sessionID)
}

func TestRemoveSessionIDError(t *testing.T) {
	// message is too short to contain session ID
	msg := []byte("test")
	_, _, err := extractSessionID(msg)
	require.Error(t, err)
}

func TestSuffixSessionIDError(t *testing.T) {
	msg := make([]byte, MaxDatagramFrameSize-sessionIDLen)
	_, err := suffixSessionID(testSessionID, msg)
	require.NoError(t, err)

	msg = make([]byte, MaxDatagramFrameSize-sessionIDLen+1)
	_, err = suffixSessionID(testSessionID, msg)
	require.Error(t, err)
}

func TestDatagram(t *testing.T) {
	maxPayload := make([]byte, maxDatagramPayloadSize)
	noPayloadSession := uuid.New()
	maxPayloadSession := uuid.New()
	sessionToPayload := []*SessionDatagram{
		{
			ID:      noPayloadSession,
			Payload: make([]byte, 0),
		},
		{
			ID:      maxPayloadSession,
			Payload: maxPayload,
		},
	}
	flowPayloads := [][]byte{
		maxPayload,
	}

	testDatagram(t, 1, sessionToPayload, nil)
	testDatagram(t, 2, sessionToPayload, flowPayloads)
}

func testDatagram(t *testing.T, version uint8, sessionToPayloads []*SessionDatagram, packetPayloads [][]byte) {
	quicConfig := &quic.Config{
		KeepAlivePeriod:      5 * time.Millisecond,
		EnableDatagrams:      true,
		MaxDatagramFrameSize: MaxDatagramFrameSize,
	}
	quicListener := newQUICListener(t, quicConfig)
	defer quicListener.Close()

	logger := zerolog.Nop()

	errGroup, ctx := errgroup.WithContext(context.Background())
	// Run edge side of datagram muxer
	errGroup.Go(func() error {
		// Accept quic connection
		quicSession, err := quicListener.Accept(ctx)
		if err != nil {
			return err
		}

		sessionDemuxChan := make(chan *SessionDatagram, 16)

		switch version {
		case 1:
			muxer := NewDatagramMuxer(quicSession, &logger, sessionDemuxChan)
			muxer.ServeReceive(ctx)
		case 2:
			packetDemuxChan := make(chan []byte, len(packetPayloads))
			muxer := NewDatagramMuxerV2(quicSession, &logger, sessionDemuxChan, packetDemuxChan)
			muxer.ServeReceive(ctx)

			for _, expectedPayload := range packetPayloads {
				require.Equal(t, expectedPayload, <-packetDemuxChan)
			}
		default:
			return fmt.Errorf("unknown datagram version %d", version)
		}

		for _, expectedPayload := range sessionToPayloads {
			actualPayload := <-sessionDemuxChan
			require.Equal(t, expectedPayload, actualPayload)
		}
		return nil
	})

	largePayload := make([]byte, MaxDatagramFrameSize)
	// Run cloudflared side of datagram muxer
	errGroup.Go(func() error {
		tlsClientConfig := &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"argotunnel"},
		}
		// Establish quic connection
		quicSession, err := quic.DialAddrEarly(quicListener.Addr().String(), tlsClientConfig, quicConfig)
		require.NoError(t, err)
		defer quicSession.CloseWithError(0, "")

		// Wait a few milliseconds for MTU discovery to take place
		time.Sleep(time.Millisecond * 100)

		var muxer BaseDatagramMuxer
		switch version {
		case 1:
			muxer = NewDatagramMuxer(quicSession, &logger, nil)
		case 2:
			muxerV2 := NewDatagramMuxerV2(quicSession, &logger, nil, nil)
			for _, payload := range packetPayloads {
				require.NoError(t, muxerV2.MuxPacket(payload))
			}
			// Payload larger than transport MTU, should not be sent
			require.Error(t, muxerV2.MuxPacket(largePayload))
			muxer = muxerV2
		default:
			return fmt.Errorf("unknown datagram version %d", version)
		}

		for _, sessionDatagram := range sessionToPayloads {
			require.NoError(t, muxer.MuxSession(sessionDatagram.ID, sessionDatagram.Payload))
		}
		// Payload larger than transport MTU, should not be sent
		require.Error(t, muxer.MuxSession(testSessionID, largePayload))

		// Wait for edge to finish receiving the messages
		time.Sleep(time.Millisecond * 100)

		return nil
	})

	require.NoError(t, errGroup.Wait())
}

func newQUICListener(t *testing.T, config *quic.Config) quic.Listener {
	// Create a simple tls config.
	tlsConfig := generateTLSConfig()

	listener, err := quic.ListenAddr("127.0.0.1:0", tlsConfig, config)
	require.NoError(t, err)

	return listener
}

func generateTLSConfig() *tls.Config {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		panic(err)
	}
	template := x509.Certificate{SerialNumber: big.NewInt(1)}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		panic(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		NextProtos:   []string{"argotunnel"},
	}
}

type sessionMuxer interface {
	SendToSession(sessionID uuid.UUID, payload []byte) error
}

type mockSessionReceiver struct {
	expectedSessionToPayload map[uuid.UUID][]byte
	receivedCount            int
}

func (msr *mockSessionReceiver) ReceiveDatagram(sessionID uuid.UUID, payload []byte) error {
	expectedPayload := msr.expectedSessionToPayload[sessionID]
	if !bytes.Equal(expectedPayload, payload) {
		return fmt.Errorf("expect %v to have payload %s, got %s", sessionID, string(expectedPayload), string(payload))
	}
	msr.receivedCount++
	return nil
}

type mockFlowReceiver struct {
	expectedPayloads [][]byte
	receivedCount    int
}

func (mfr *mockFlowReceiver) ReceiveFlow(payload []byte) error {
	expectedPayload := mfr.expectedPayloads[mfr.receivedCount]
	if !bytes.Equal(expectedPayload, payload) {
		return fmt.Errorf("expect flow %d to have payload %s, got %s", mfr.receivedCount, string(expectedPayload), string(payload))
	}
	mfr.receivedCount++
	return nil
}
