package minecraft

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login/jwt"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"github.com/sandertv/gophertunnel/minecraft/resource"
	"io"
	"net"
	"sync"
	"time"
)

// Conn represents a Minecraft (Bedrock Edition) connection over a specific net.Conn transport layer. Its
// methods (Read, Write etc.) are safe to be called from multiple goroutines simultaneously.
type Conn struct {
	conn    net.Conn
	pool    packet.Pool
	encoder *packet.Encoder
	decoder *packet.Decoder

	// privateKey is the private key of this end of the connection. Each connection, regardless of which side
	// the connection is on, server or client, has a unique private key generated.
	privateKey *ecdsa.PrivateKey
	// salt is a 16 byte long randomly generated byte slice which is only used if the Conn is a server sided
	// connection. It is otherwise left unused.
	salt []byte

	// packets is a channel of byte slices containing serialised packets that are coming in from the other
	// side of the connection.
	packets      chan []byte
	readDeadline <-chan time.Time

	sendMutex sync.Mutex
	// bufferedSend is a slice of byte slices containing packets that are 'written'. They are buffered until
	// they are sent each 20th of a second.
	bufferedSend [][]byte

	// loggedIn is a bool indicating if the connection was logged in. It is set to true after the entire login
	// sequence is completed.
	loggedIn bool
	// expectedID is the ID of the next packet expected during the login sequence. The value becomes
	// irrelevant when loggedIn is true.
	expectedID uint32

	// resourcePacks is a slice of resource packs that the listener may hold. Each client will be asked to
	// download these resource packs upon joining.
	resourcePacks []*resource.Pack
	// texturePacksRequired specifies if clients that join must accept the texture pack in order for them to
	// be able to join the server. If they don't accept, they can only leave the server.
	texturePacksRequired bool

	packQueue *resourcePackQueue

	close chan bool
}

// newConn creates a new Minecraft connection for the net.Conn passed, reading and writing compressed
// Minecraft packets to that net.Conn.
func newConn(conn net.Conn) *Conn {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	c := &Conn{
		conn:    conn,
		encoder: packet.NewEncoder(conn),
		decoder: packet.NewDecoder(conn),
		pool:    packet.NewPool(),
		packets: make(chan []byte, 32),
		close:   make(chan bool, 1),
		// By default we set this to the login packet, but a client will have to set the play status packet's
		// ID as the first expected one.
		expectedID: packet.IDLogin,
		privateKey: key,
		salt:       make([]byte, 16),
	}
	_, _ = rand.Read(c.salt)

	go func() {
		ticker := time.NewTicker(time.Second / 20)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := c.Flush(); err != nil {
					return
				}
			case <-c.close:
				// Break out of the goroutine and propagate the close signal again.
				c.close <- true
				return
			}
		}
	}()
	return c
}

// WritePacket encodes the packet passed and writes it to the Conn. The encoded data is buffered until the
// next 20th of a second, after which the data is flushed and sent over the connection.
func (conn *Conn) WritePacket(pk packet.Packet) error {
	header := &packet.Header{PacketID: pk.ID()}
	buffer := bytes.NewBuffer(make([]byte, 0, 5))
	if err := header.Write(buffer); err != nil {
		return fmt.Errorf("error writing packet header: %v", err)
	}
	pk.Marshal(buffer)
	_, err := conn.Write(buffer.Bytes())
	return err
}

// ReadPacket reads a packet from the Conn, depending on the packet ID that is found in front of the packet
// data. If a read deadline is set, an error is returned if the deadline is reached before any packet is
// received.
// The packet received must not be held until the next packet is read using ReadPacket(). If the same type of
// packet is read, the previous one will be invalidated.
//
// If the packet read was not implemented, a *packet.Unknown is returned, containing the raw payload of the
// packet read.
func (conn *Conn) ReadPacket() (pk packet.Packet, err error) {
	select {
	case data := <-conn.packets:
		buf := bytes.NewBuffer(data)
		header := &packet.Header{}
		if err := header.Read(buf); err != nil {
			return nil, fmt.Errorf("error reading packet header: %v", err)
		}
		// Attempt to fetch the packet with the right packet ID from the pool.
		pk, ok := conn.pool[header.PacketID]
		if !ok {
			// We haven't implemented this packet ID, so we return an unknown packet which could be used by
			// the reader.
			pk = &packet.Unknown{PacketID: header.PacketID}
		}
		// Unmarshal the bytes into the packet and return the error.
		return pk, pk.Unmarshal(buf)
	case <-conn.readDeadline:
		return nil, fmt.Errorf("error reading packet: read timeout")
	case <-conn.close:
		conn.close <- true
		return nil, fmt.Errorf("error reading packet: connection closed")
	}
}

// Write writes a slice of serialised packet data to the Conn. The data is buffered until the next 20th of a
// tick, after which it is flushed to the connection. Write returns the amount of bytes written n.
func (conn *Conn) Write(b []byte) (n int, err error) {
	conn.sendMutex.Lock()
	defer conn.sendMutex.Unlock()

	conn.bufferedSend = append(conn.bufferedSend, b)
	return len(b), nil
}

// Read reads a packet from the connection into the byte slice passed, provided the byte slice is big enough
// to carry the full packet.
// It is recommended to use ReadPacket() rather than Read() in cases where reading is done directly.
func (conn *Conn) Read(b []byte) (n int, err error) {
	select {
	case data := <-conn.packets:
		if len(b) < len(data) {
			return 0, fmt.Errorf("error reading data: A message sent on a Minecraft socket was larger than the buffer used to receive the message into")
		}
		return copy(b, data), nil
	case <-conn.readDeadline:
		return 0, fmt.Errorf("error reading packet: read timeout")
	case <-conn.close:
		conn.close <- true
		return 0, fmt.Errorf("error reading packet: connection closed")
	}
}

// Flush flushes the packets currently buffered by the connections to the underlying net.Conn, so that they
// are directly sent.
func (conn *Conn) Flush() error {
	conn.sendMutex.Lock()
	defer conn.sendMutex.Unlock()

	if len(conn.bufferedSend) > 0 {
		if err := conn.encoder.Encode(conn.bufferedSend); err != nil {
			return fmt.Errorf("error encoding packet batch: %v", err)
		}
		// Reset the send slice so that we don't accidentally send the same packets.
		conn.bufferedSend = nil
	}
	return nil
}

// Close closes the Conn and its underlying connection. Before closing, it also calls Flush() so that any
// packets currently pending are sent out.
func (conn *Conn) Close() error {
	if len(conn.close) != 0 {
		// The connection was already closed, no need to do anything.
		return nil
	}
	_ = conn.Flush()
	conn.close <- true
	return conn.conn.Close()
}

// LocalAddr returns the local address of the underlying connection.
func (conn *Conn) LocalAddr() net.Addr {
	return conn.conn.LocalAddr()
}

// RemoteAddr returns the remote address of the underlying connection.
func (conn *Conn) RemoteAddr() net.Addr {
	return conn.conn.RemoteAddr()
}

// SetDeadline sets the read and write deadline of the connection. It is equivalent to calling SetReadDeadline
// and SetWriteDeadline at the same time.
func (conn *Conn) SetDeadline(t time.Time) error {
	return conn.SetReadDeadline(t)
}

// SetReadDeadline sets the read deadline of the Conn to the time passed. The time must be after time.Now().
// Passing an empty time.Time to the method (time.Time{}) results in the read deadline being cleared.
func (conn *Conn) SetReadDeadline(t time.Time) error {
	if t.Before(time.Now()) {
		return fmt.Errorf("error setting read deadline: time passed is before time.Now()")
	}
	empty := time.Time{}
	if t == empty {
		// Empty time, so we just set the time to some crazy high value to ensure the read deadline is never
		// actually reached.
		conn.readDeadline = time.After(time.Hour * 1000000)
	} else {
		conn.readDeadline = time.After(t.Sub(time.Now()))
	}
	return nil
}

// SetWriteDeadline is a stub function to implement net.Conn. It has no functionality.
func (conn *Conn) SetWriteDeadline(t time.Time) error {
	return nil
}

// handleIncoming handles an incoming serialised packet from the underlying connection. If the connection is
// not yet logged in, the packet is immediately read and processed.
func (conn *Conn) handleIncoming(data []byte) error {
	conn.packets <- data
	if !conn.loggedIn {
		pk, err := conn.ReadPacket()
		if err != nil {
			return err
		}
		if pk.ID() != conn.expectedID {
			// This is not the packet we expected next in the login sequence. We just ignore it as it might
			// be a packet such as a movement that was simply sent too early.
			return nil
		}
		switch pk := pk.(type) {
		// Internal packets destined for the server.
		case *packet.Login:
			return conn.handleLogin(pk)
		case *packet.ClientToServerHandshake:
			return conn.handleClientToServerHandshake(pk)
		case *packet.ResourcePackClientResponse:
			return conn.handleResourcePackClientResponse(pk)
		case *packet.ResourcePackChunkRequest:
			return conn.handleResourcePackChunkRequest(pk)

		// Internal packets destined for the client.
		case *packet.PlayStatus:
			return conn.handlePlayStatus(pk)
		case *packet.Disconnect:
			return conn.Close()
		}
	}
	return nil
}

// handleLogin handles an incoming login packet. It verifies an decodes the login request found in the packet
// and returns an error if it couldn't be done successfully.
func (conn *Conn) handleLogin(pk *packet.Login) error {
	// The next expected packet is a response from the client to the handshake.
	conn.expectedID = packet.IDClientToServerHandshake

	if pk.ClientProtocol != protocol.CurrentProtocol {
		// By default we assume the client is outdated.
		status := packet.PlayStatusLoginFailedClient
		if pk.ClientProtocol > protocol.CurrentProtocol {
			// The server is outdated in this case, so we have to change the status we send.
			status = packet.PlayStatusLoginFailedServer
		}
		_ = conn.WritePacket(&packet.PlayStatus{Status: status})
		return conn.Close()
	}

	publicKey, authenticated, err := login.Verify(pk.ConnectionRequest)
	if err != nil {
		return fmt.Errorf("error verifying login request: %v", err)
	}
	if !authenticated {
		return fmt.Errorf("connection %v was not authenticated to XBOX Live", conn.RemoteAddr())
	}
	identityData, clientData, err := login.Decode(pk.ConnectionRequest)
	if err != nil {
		return fmt.Errorf("error decoding login request: %v", err)
	}
	// First validate the identity data and the client data to ensure we're working with valid data. Mojang
	// might change this data, or some custom client might fiddle with the data, so we can never be too sure.
	if err := identityData.Validate(); err != nil {
		return fmt.Errorf("invalid identity data: %v", err)
	}
	if err := clientData.Validate(); err != nil {
		return fmt.Errorf("invalid client data: %v", err)
	}
	if err := conn.enableEncryption(publicKey); err != nil {
		return fmt.Errorf("error enabling encryption: %v", err)
	}
	return nil
}

// handleClientToServerHandshake handles an incoming ClientToServerHandshake packet.
func (conn *Conn) handleClientToServerHandshake(*packet.ClientToServerHandshake) error {
	// The next expected packet is a resource pack client response.
	conn.expectedID = packet.IDResourcePackClientResponse

	if err := conn.WritePacket(&packet.PlayStatus{Status: packet.PlayStatusLoginSuccess}); err != nil {
		return fmt.Errorf("error sending play status login success: %v", err)
	}
	pk := &packet.ResourcePacksInfo{TexturePackRequired: conn.texturePacksRequired}
	for _, pack := range conn.resourcePacks {
		resourcePack := packet.ResourcePack{UUID: pack.UUID(), Version: pack.Version(), Size: int64(pack.Len())}
		if pack.HasScripts() {
			// One of the resource packs has scripts, so we set HasScripts in the packet to true.
			pk.HasScripts = true
			resourcePack.HasScripts = true
		}
		// If it has behaviours, add it to the behaviour pack list. If not, we add it to the texture packs
		// list.
		if pack.HasBehaviours() {
			pk.BehaviourPacks = append(pk.BehaviourPacks, resourcePack)
			continue
		}
		pk.TexturePacks = append(pk.TexturePacks, resourcePack)
	}
	// Finally we send the packet after the play status.
	if err := conn.WritePacket(pk); err != nil {
		return fmt.Errorf("error sending resource packs info: %v", err)
	}
	return nil
}

// packChunkSize is the size of a single chunk of data from a resource pack: 512 kB or 0.5 MB
const packChunkSize = 1024 * 512

// handleResourcePackClientResponse handles an incoming resource pack client response packet. The packet is
// handled differently depending on the response.
func (conn *Conn) handleResourcePackClientResponse(pk *packet.ResourcePackClientResponse) error {
	switch pk.Response {
	case packet.PackResponseRefused:
		// Even though this response is never sent, we handle it appropriately in case it is changed to work
		// correctly again.
		return conn.Close()
	case packet.PackResponseSendPacks:
		packs := pk.PacksToDownload
		conn.packQueue = &resourcePackQueue{packs: conn.resourcePacks}
		if err := conn.packQueue.Request(packs); err != nil {
			return fmt.Errorf("error looking up resource packs to download: %v", err)
		}
		// Proceed with the first resource pack download. We run all downloads in sequence rather than in
		// parallel, as it's less prone to packet loss.
		if err := conn.nextResourcePackDownload(); err != nil {
			return err
		}
	case packet.PackResponseAllPacksDownloaded:
		pk := &packet.ResourcePackStack{TexturePackRequired: conn.texturePacksRequired}
		for _, pack := range conn.resourcePacks {
			resourcePack := packet.ResourcePack{UUID: pack.UUID(), Version: pack.Version()}
			// If it has behaviours, add it to the behaviour pack list. If not, we add it to the texture packs
			// list.
			if pack.HasBehaviours() {
				pk.BehaviourPacks = append(pk.BehaviourPacks, resourcePack)
				continue
			}
			pk.TexturePacks = append(pk.TexturePacks, resourcePack)
		}
		if err := conn.WritePacket(pk); err != nil {
			return fmt.Errorf("error writing resource pack stack packet: %v", err)
		}
	case packet.PackResponseCompleted:
		// This is as far as we can go in terms of covering up the login sequence. The next packet is the
		// StartGame packet, which includes far too many fields related to the world which we simply cannot
		// fill out in advance.
		conn.loggedIn = true
	default:
		return fmt.Errorf("unknown resource pack client response: %v", pk.Response)
	}
	return nil
}

// nextResourcePackDownload moves to the next resource pack to download and sends a resource pack data info
// packet with information about it.
func (conn *Conn) nextResourcePackDownload() error {
	pk, ok := conn.packQueue.NextPack()
	if !ok {
		return fmt.Errorf("no resource packs to download")
	}
	if err := conn.WritePacket(pk); err != nil {
		return fmt.Errorf("error sending resource pack data info packet: %v", err)
	}
	// Set the next expected packet to ResourcePackChunkRequest packets.
	conn.expectedID = packet.IDResourcePackChunkRequest
	return nil
}

// handleResourcePackChunkRequest handles a resource pack chunk request, which requests a part of the resource
// pack to be downloaded.
func (conn *Conn) handleResourcePackChunkRequest(pk *packet.ResourcePackChunkRequest) error {
	current := conn.packQueue.currentPack
	if current.UUID() != pk.UUID {
		return fmt.Errorf("resource pack chunk request had unexpected UUID: expected %v, but got %v", current.UUID(), pk.UUID)
	}
	if conn.packQueue.currentOffset != int64(pk.ChunkIndex)*packChunkSize {
		return fmt.Errorf("resource pack chunk request had unexpected chunk index: expected %v, but got %v", conn.packQueue.currentOffset/packChunkSize, pk.ChunkIndex)
	}
	response := &packet.ResourcePackChunkData{
		UUID:       pk.UUID,
		ChunkIndex: pk.ChunkIndex,
		DataOffset: conn.packQueue.currentOffset,
		Data:       make([]byte, packChunkSize),
	}
	conn.packQueue.currentOffset += packChunkSize
	// We read the data directly into the response's data.
	if n, err := current.ReadAt(response.Data, response.DataOffset); err != nil {
		// If we hit an EOF, we don't need to return an error, as we've simply reached the end of the content
		// AKA the last chunk.
		if err != io.EOF {
			return fmt.Errorf("error reading resource pack chunk: %v", err)
		}
		response.Data = response.Data[:n]

		defer func() {
			if !conn.packQueue.AllDownloaded() {
				_ = conn.nextResourcePackDownload()
			} else {
				conn.expectedID = packet.IDResourcePackClientResponse
			}
		}()
	}
	if err := conn.WritePacket(response); err != nil {
		return fmt.Errorf("error writing resource pack chunk data packet: %v", err)
	}

	return nil
}

// handlePlayStatus handles an incoming PlayStatus packet. It reacts differently depending on the status
// found in the packet.
func (conn *Conn) handlePlayStatus(pk *packet.PlayStatus) error {
	switch pk.Status {
	case packet.PlayStatusLoginSuccess:
		// TODO
	case packet.PlayStatusLoginFailedClient:
		_ = conn.Close()
		return fmt.Errorf("client outdated")
	case packet.PlayStatusLoginFailedServer:
		_ = conn.Close()
		return fmt.Errorf("server outdated")
	case packet.PlayStatusPlayerSpawn:
		// TODO
	case packet.PlayStatusLoginFailedInvalidTenant:
		_ = conn.Close()
		return fmt.Errorf("invalid edu edition game owner")
	case packet.PlayStatusLoginFailedVanillaEdu:
		_ = conn.Close()
		return fmt.Errorf("cannot join an edu edition game on vanilla")
	case packet.PlayStatusLoginFailedEduVanilla:
		_ = conn.Close()
		return fmt.Errorf("cannot join a vanilla game on edu edition")
	case packet.PlayStatusLoginFailedServerFull:
		_ = conn.Close()
		return fmt.Errorf("server full")
	default:
		return fmt.Errorf("unknown play status in PlayStatus packet %v", pk.Status)
	}
	return nil
}

// enableEncryption enables encryption on the server side over the connection. It sends an unencrypted
// handshake packet to the client and enables encryption after that.
func (conn *Conn) enableEncryption(clientPublicKey *ecdsa.PublicKey) error {
	pubKey, err := jwt.MarshalPublicKey(&conn.privateKey.PublicKey)
	if err != nil {
		return fmt.Errorf("error marshaling public key: %v", err)
	}
	header := jwt.Header{
		Algorithm: "ES384",
		X5U:       pubKey,
	}
	payload := map[string]interface{}{
		"salt": base64.RawStdEncoding.EncodeToString(conn.salt),
	}

	// We produce an encoded JWT using the header and payload above, then we send the JWT in a ServerToClient-
	// Handshake packet so that the client can initialise encryption.
	serverJWT, err := jwt.New(header, payload, conn.privateKey)
	if err != nil {
		return fmt.Errorf("error creating encoded JWT: %v", err)
	}
	if err := conn.WritePacket(&packet.ServerToClientHandshake{JWT: serverJWT}); err != nil {
		return fmt.Errorf("error sending ServerToClientHandshake packet: %v", err)
	}
	// Flush immediately as we'll enable encryption after this.
	_ = conn.Flush()

	// We first compute the shared secret.
	clientX, clientY := clientPublicKey.X, clientPublicKey.Y
	x, _ := clientPublicKey.Curve.ScalarMult(clientX, clientY, conn.privateKey.D.Bytes())
	sharedSecret := x.Bytes()
	keyBytes := sha256.Sum256(append(conn.salt, sharedSecret...))

	// Finally we enable encryption for the encoder and decoder using the secret key bytes we produced.
	conn.encoder.EnableEncryption(keyBytes)
	conn.decoder.EnableEncryption(keyBytes)

	return nil
}
