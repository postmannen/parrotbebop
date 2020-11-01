// The latest version of the ardrone3.xml document can be found at
// https://github.com/Parrot-Developers/arsdk-xml/tree/master/xml

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"reflect"
	"strconv"
	"time"
	"unsafe"

	"github.com/eiannone/keyboard"
)

// Drone holds the data and methods specific for the drone.
type Drone struct {
	// The ip address of the drone
	addressDrone string
	// Used for initializing the connection to the drone over TCP.
	portDiscover string
	// Controller to drone, port the controller wil send the drone messages on.
	portC2D string
	// Drone to controller, port the controller will listen on for drone messages.
	portD2C        string
	portRTPStream  string
	portRTPControl string
	// Channel to put the raw UDP packages from the drone.
	chReceivedUDPPacket chan networkUDPPacket
	// Channel to put the raw UDP packages to be sent to the drone.
	chSendingUDPPacket chan networkUDPPacket
	// Channel to put the inputAction type send to the drone when
	// for example a key is pressed on the keyboard.
	chInputActions chan inputAction
	// Sending to this channel will quit the controller program.
	chQuit chan struct{}
	// Sending to this channel will disconnect all network related
	// go routines, and then reconnect to the drone.
	chNetworkConnect chan struct{}
	// chPcmdPacketScheduler is used to set the frequency of PcmdPacket's
	// that will be sent from the controller to the drone.
	// All Pcmd packets from the controller should go through here to not
	// overwhelm the drone with to many commands which can interupt
	// other commands.
	chPcmdPacketScheduler chan networkUDPPacket
	// The conn object for the UDP network listener
	connUDPRead net.PacketConn
	// The conn object for the UDP connection to send commands to
	// the drone.
	connUDPWrite *net.UDPConn
	// Piloting Command
	pcmd Ardrone3PilotingPCMDArguments
}

// NewDrone will initalize all the variables needed for a drone,
// like ports used, ip adresses, etc.
func NewDrone() *Drone {
	return &Drone{
		addressDrone: "192.168.42.1",
		portDiscover: "44444",
		//portC2D:        "54321", // This one is now assigned via discovery
		portD2C:        "43210",
		portRTPStream:  "55004",
		portRTPControl: "55005",

		chReceivedUDPPacket: make(chan networkUDPPacket),
		chSendingUDPPacket:  make(chan networkUDPPacket),
		chInputActions:      make(chan inputAction),
		chQuit:              make(chan struct{}),
		chNetworkConnect:    make(chan struct{}),
		// Creating a buffer of 100 here which should mean that
		// it can buffer up commands for the next 5 seconds since
		// pcmd commands are onyl sent each 50 milli second.
		//
		// NB: Not sure how this works out, so it might need to be
		// adjusted or put to 0.
		chPcmdPacketScheduler: make(chan networkUDPPacket, 100),

		pcmd: Ardrone3PilotingPCMDArguments{
			Flag:               0,
			Roll:               0,
			Pitch:              0,
			Yaw:                0,
			Gaz:                0,
			TimestampAndSeqNum: 0,
		},
	}
}

// Discover will initalize the connection with the drone.
func (d *Drone) Discover() error {
	// A discover with JSON formated data like :
	//
	// { "status": 0, "c2d_port": 54321, "c2d_update_port": 51, "c2d_user_port": 21, "qos_mode": 0, "arstream2_server_stream_port": 5004, "arstream2_server_control_port": 5005 }

	//const addr = "192.168.42.1:44444"

	nd := net.Dialer{Timeout: time.Second * 3, Cancel: d.chQuit}
	discoverConn, err := nd.Dial("tcp", d.addressDrone+":"+d.portDiscover)
	if err != nil {
		return err
	}

	defer func() {
		err := discoverConn.Close()
		if err != nil {
			log.Printf("error: failed to close discoverConn: %v\r\n", err)
		}
		log.Printf("...closed discoverConn\r\n")
	}()

	// The drone expects the discovery data payload in the following format.
	_, err = discoverConn.Write(
		[]byte(
			fmt.Sprintf(`{
						"controller_type": "computer",
						"controller_name": "go-bebop",
						"d2c_port": "%s",
						"arstream2_client_stream_port": "%s",
						"arstream2_client_control_port": "%s",
						}`,
				d.portD2C,
				d.portRTPStream,
				d.portRTPControl),
		),
	)
	if err != nil {
		log.Println("error: Discover, discoveryClient.Write: ", err)
	}

	data := make([]byte, 1024) // not quite sure about the size here...

	// Read the returned response of the discovery from the drone.
	_, err = discoverConn.Read(data)
	if err != nil {
		return err
	}
	log.Printf("*** Discovery data \r\n %v \r\n\r\n, Size of data = %v\r\n", string(data), len(data))

	// Using anonymous struct just for unmarshalling the discoveryData
	discoverData := struct {
		Status                     int `json:"status"`
		C2dPort                    int `json:"c2d_port"`
		C2dUpdate                  int `json:"c2d_update_port"`
		C2dUserPort                int `json:"c2d_user_port"`
		QosMode                    int `json:"qos_mode"`
		Arstream2ServerStreamPort  int `json:"arstream2_server_stream_port"`
		Arstream2ServerControlPort int `json:"arstream2_server_control_port"`
	}{}

	// Remove all the zero allocations in the byte slice, else unmarshal will fail.
	data = bytes.Trim(data, "\x00")

	if err := json.Unmarshal(data, &discoverData); err != nil {
		log.Println("error:Umarshal discovery data: ", err)
	}
	fmt.Printf("Unmarshaled : %v\r\n", discoverData)

	// if the status !=0 the disovery failed.
	if discoverData.Status != 0 {
		log.Fatal("DISCOVERY FAILED")
	}

	// Set the received Controller to Drone port to use based on discovery data.
	d.portC2D = strconv.Itoa(discoverData.C2dPort)

	return nil
}

// // getNetworkTestingPacketsD2C gets the raw UDP packets from the test data.
// // Will read the raw testing UDP packets, and put them on a channel to be
// // picked up by the frame decoder.
// //
// func (d *Drone) readNetworkUDPTestingPacketsD2C() {
// 	/* More packets to put into buf if needed.
// 	2, 127, 28, 38, 0, 0, 0, 1, 4, 9, 0, 0, 0, 0, 0, 0, 64, 127, 64, 0, 0, 0, 0, 0, 64, 127, 64, 0, 0, 0, 0, 0, 64, // 127, 64, 83, 83, 83,
//
// 	2, 127, 32, 13, 0, 0, 0, 1, 25, 0, 0, 243, 0,
// 	*/
// 	// the simulated testing data to use for reading
// 	buf := []byte{
// 		2, 127, 8, 23, 0, 0, 0, 1, 4, 6, 0, 154, 221, 45, 61, 44, 209, 73, 188, 121, 230, 52, 64}
//
// 	p := networkUDPPacket{
// 		size: len(buf),
// 		data: buf,
// 		// Since this is a new UDP packet, and we want to start reading
// 		// the first frame from the start we set the start position to 0.
// 		framePos: 0,
// 	}
//
// 	// send the packet received over a channel to later parse out ARNetworkAL/frames.
// 	d.chReceivedUDPPacket <- p
//
// }

// getNetworkPacketsD2C gets the raw UDP packets from the drone sent to the controller.
// Will read the raw UDP packets from the network, and put them on a channel to be
// picked up by the frame decoder.
func (d *Drone) readNetworkUDPPacketsD2C(ctx context.Context) {

	defer func() {
		err := d.connUDPRead.Close()
		if err != nil {
			log.Printf("error: failed to close connUDPRead: %v\r\n", err)
		}
		log.Printf("...closed connUDPRead\r\n")
	}()

	for {
		select {
		case <-ctx.Done():
			log.Printf("info: exiting readNetworkUDPPacketD2C\n")
			return
		default:
			p := make([]byte, 16384) // NB: buf might be to small ?

			n, addr, err := d.connUDPRead.ReadFrom(p)
			if err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) {
					d.chNetworkConnect <- struct{}{}
					return
				}
				log.Printf("error: failed ReadFrom: %v %v\n", addr, err)
			}

			// setting the deadline after a succesful write will make the
			// next read fail if it does not receive any data within the
			// deadline
			d.connUDPRead.SetReadDeadline(time.Now().Add(time.Second * 3))

			packet := networkUDPPacket{
				size: n,
				data: p,
				// Set framePos to zero so we start with the first frame.
				framePos: 0,
			}

			// send the packet received over a channel to later parse out ARNetworkAL/frames.
			d.chReceivedUDPPacket <- packet

		}
	}
}

// writeNetworkPacketsC2D writes the raw UDP packets from the controller to the drone.
// Will receive []byte packet to write on an incomming channel for the function.
func (d *Drone) writeNetworkUDPPacketsC2D(ctx context.Context) {

	defer func() {
		err := d.connUDPWrite.Close()
		if err != nil {
			log.Printf("error:failed to close connUDPWrite: %v\r\n", err)
		}
		fmt.Printf("...connUDPWrite closed\r\n")
	}()

	for {
		select {
		case <-ctx.Done():
			log.Printf("info: exiting writeNetworkUDPPacketsC2D\n")
			return
		case v := <-d.chSendingUDPPacket:

			fmt.Printf("sending to Drone, v = %v\r\n", v.data)

			n, err := d.connUDPWrite.Write(v.data)
			if err != nil {
				log.Printf("error: failed conn.Write while sending: %v", err)
			}

			fmt.Printf("*** while sending to Drone, n = %v\r\n", n)
			fmt.Printf("--------------------\r\n")
			//time.Sleep(time.Millisecond * 200)
		}
	}
}

// handleReadPackages holds the logic for what action to do when an UDP
// packet is receied and what to do based on the content of the package.
// This means sending a pong for a received package, or do some action
// if a state command where received from the drone.
func (d *Drone) handleReadPackages(packetCreator *udpPacketCreator, ctx context.Context) error {
	// Loop, get a recieved UDP packet from the channel, and decode it.
	for {
		select {
		case <-ctx.Done():
			log.Printf("info: exiting handleReadPAclages\n")
			return fmt.Errorf("error: context.Done() for handleReadPackages")
		default:
			// Get a packet
			udpPacket := <-d.chReceivedUDPPacket

			var lastFrame bool
			// An UDP Packet can consist of several frames, loop over each
			// frame found in the packet. If last frame is found, break out.
			for {
				// decode will decode a whole UDP packet given as input,
				// and return a frame of the ARNetworkAL protocol, it will
				// return error== io.EOF when decoding of the whole packet
				// is done. If the there are more than one ARNetworkAL frame
				// in the UDP packet the method will return error == nil,
				// and the method should be run over again until io.EOF is
				// received.
				frameARNetworkAL, err := udpPacket.decode()

				// Check if it was the last frame in the UDP packet.
				if err == io.EOF {
					lastFrame = true
				}

				// • Ack(1): Acknowledgment of previously received data
				// • Data(2): Normal data (no ack requested)
				// • Low latency data(3): Treated as normal data on the network, but are
				//   given higher priority internally
				// • Data with ack(4): Data requesting an ack. The receiver must send an
				//   ack for this data !

				// The drone will send out ping packets each second where we will need to
				// reply with a pong. The drone will assume the connection is broken if a
				// pong is not received within 5 seconds.
				// Check if it is a ping packet from drone, and incase
				// it is, reply with a pong.
				if frameARNetworkAL.targetBufferID == 0 || frameARNetworkAL.targetBufferID == 1 {
					{
						p := packetCreator.encodePong(frameARNetworkAL)
						d.chSendingUDPPacket <- p
					}

					if lastFrame {
						break
					}

					continue
				}

				// Send an ACK packet if the dataType == 4
				if frameARNetworkAL.dataType == 4 {
					{
						p := packetCreator.encodeAck(frameARNetworkAL.targetBufferID, uint8(frameARNetworkAL.sequenceNR))
						d.chSendingUDPPacket <- p
					}
				}

				// Try to figure out what kind of command that where received.
				// Based on the type of cmdArgs we can execute som action.
				cmd, cmdArgs, err := frameARNetworkAL.decode()
				if err != nil {
					log.Println("error: frame.decode: ", err)
					break
				}
				fmt.Printf("----------COMMAND-------------------------------------------\r\n")
				fmt.Printf("-- cmd = %+v\r\n", cmd)
				fmt.Printf("-- Value of cmdArgs = %+v\r\n", cmdArgs)
				fmt.Printf("-- Type of cmdArgs = %+T\r\n", cmdArgs)
				switch cmdArgs.(type) {
				case Ardrone3CameraStateOrientationArguments:
					//log.Printf("** EXECUTING ACTION FOR TYPE, Ardrone3CameraStateOrientationArguments ...........\r\n")
				case Ardrone3PilotingStateAttitudeChangedArguments:
					//log.Printf("** EXECUTING ACTION FOR TYPE, Ardrone3PilotingStateAttitudeChangedArguments\r\n")
				}
				fmt.Printf("-----------------------------------------------------------\r\n")

				// If no more frames, break out of for loop to read
				// the next package received.
				if lastFrame {
					break
				}
			}
		}
	}
}

// TODO: Check if the inputActions can be taken from the
// commandStructure.go document, or if we will be better
// off defining them here...or if we don't need them at
// all since we can
//
// Instead of all the input definition constants below, we
// could use the already defined constants present in the
// commandStructure.go file, like..
// const CmdStopPilotedPOI CmdDef = 13 ???

// actions, the idea here is to send the actions on a keypress,
// and then have some logic who reads the actions received over
// a channel, and then do the logic for landing/takeoff/rotate etc.

type inputAction int

const (
	// Standard actions.
	//
	ActionPcmdFlag                inputAction = iota
	ActionPcmdRollLeft            inputAction = iota
	ActionPcmdRollRight           inputAction = iota
	ActionPcmdPitchForward        inputAction = iota
	ActionPcmdPitchBackward       inputAction = iota
	ActionPcmdYawClockwise        inputAction = iota
	ActionPcmdYawCounterClockwise inputAction = iota
	ActionPcmdGazInc              inputAction = iota
	ActionPcmdGazDec              inputAction = iota
	ActionTakeoff                 inputAction = iota
	ActionLanding                 inputAction = iota
	ActionEmergency               inputAction = iota
	ActionNavigateHome            inputAction = iota // Check how to implement it in xml line 153
	ActionMoveBy                  inputAction = iota // Check how to implement it in xml line 181
	ActionUserTakeoff             inputAction = iota
	ActionMoveTo                  inputAction = iota // Check how to implement it in xml line 259
	ActionCancelMoveTo            inputAction = iota
	ActionStartPilotedPOI         inputAction = iota
	ActionStopPilotedPOI          inputAction = iota
	ActionCancelMoveBy            inputAction = iota

	// Custom actions.
	//
	ActionHow inputAction = iota
	// Flattrim should be performed before a takeoff
	// to calibrate the drone.
	ActionFlatTrim inputAction = iota
	// TODO: Also check out the <class name="PilotingSettings" id="2">"
	// starting at line 1400 in the ardrone3.xml document, for more
	// commands to eventually implement.
)

// readKeyBoardEvent will read keys pressed on the keyboard,
// and pass on the correct action to be executed.
//
// TODO: Make more source to create inputActions than keyboard...
// Geofencing ?
// Map route ?
func (d *Drone) readKeyBoardEvent(ctx context.Context) {

	keysEvents, err := keyboard.GetKeys(10)
	if err != nil {
		panic(err)
	}
	defer func() {
		err := keyboard.Close()
		if err != nil {
			log.Printf("error: failed to close keyboard: %v\n", err)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			log.Printf("info: exiting readKeyBoardEvent")
			return
		case event := <-keysEvents:

			if event.Err != nil {
				panic(event.Err)
			}

			switch {
			case event.Key == keyboard.KeyEsc:
				d.chQuit <- struct{}{}
			case event.Rune == 'q':
				// Initiate a reconnect of the network.
				d.chNetworkConnect <- struct{}{}
			case event.Rune == 't':
				d.chInputActions <- ActionTakeoff
			case event.Rune == 'l':
				d.chInputActions <- ActionLanding
			case event.Key == keyboard.KeyArrowUp:
				// Up
				d.chInputActions <- ActionPcmdGazInc
			case event.Key == keyboard.KeyArrowDown:
				// Down
				d.chInputActions <- ActionPcmdGazDec
			}
		}

	}

}

// handleInputAction is where we specify what package to send to the drone
// based on what action came out of the readKeyboardEvent method.
//
// The reason we have this function and don't encode the packets directly
// in readKeyBoardEvent, is that we might want to have other input methods
// then the keyboard to control the drone.
// This function will execute the commands that arrives on the d.chInputActions.
func (d *Drone) handleInputAction(packetCreator udpPacketCreator, ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			log.Println("info: exiting handleInputAction")
			return

		case action := <-d.chInputActions:
			switch action {
			case ActionTakeoff:
				p := packetCreator.encodeCmd(Command(PilotingTakeOff), &Ardrone3PilotingTakeOffArguments{})
				d.chSendingUDPPacket <- p
			case ActionLanding:
				p := packetCreator.encodeCmd(Command(PilotingLanding), &Ardrone3PilotingLandingArguments{})
				d.chSendingUDPPacket <- p
			case ActionPcmdGazInc:
				d.pcmd.Gaz++
				d.pcmd.Gaz = d.CheckLimitPcmdField(d.pcmd.Gaz)
				arg := &Ardrone3PilotingPCMDArguments{
					Gaz: d.pcmd.Gaz,
				}
				d.chPcmdPacketScheduler <- packetCreator.encodeCmd(Command(PilotingPCMD), arg)
			case ActionPcmdGazDec:
				d.pcmd.Gaz--
				d.pcmd.Gaz = d.CheckLimitPcmdField(d.pcmd.Gaz)
				arg := &Ardrone3PilotingPCMDArguments{
					Gaz: d.pcmd.Gaz,
				}
				d.chPcmdPacketScheduler <- packetCreator.encodeCmd(Command(PilotingPCMD), arg)
			}
		}

	}
}

// PcmdPacketScheduler
// The idea here is for every time.After we check if there
// is a new received packet. If there is we passing it along
// on the d.chSendingUDPPacket channel, if there is nothing
// we just nothing and loop again.
func (d *Drone) PcmdPacketScheduler(ctx context.Context) {
	duration1 := time.Duration(50) * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			log.Println("info: exiting PcmdPacketScheduler")
			return
		case <-time.After(duration1):

			select {
			case p := <-d.chPcmdPacketScheduler:
				d.chSendingUDPPacket <- p
			default:
				log.Printf("No packets to send, or buffer full\n")
			}
		}

	}

}

// CheckLimitPcmdField Will check if the number is within the
// correct limits, if above or below it will be adjusted, and
// the adjusted value will be returned.
// If it is within it's limits, it will be returned as is.
func (d *Drone) CheckLimitPcmdField(number int8) int8 {
	switch {
	case number > 100:
		return 100
	case number < -100:
		return -100
	default:
		break
	}

	return number
}

// TODO: The Pcmd structure below are really not necessary since
// since it already exist in the commandStructure.go file with
// the correct type for all the struct fields. This also go for
// all the other command types for the drone.
//
// Thinking maybe adding an encode method to all types that do not
// include the word state in it's name, since state is generally
// just messages from the drone to the controller.
//
// This should mean we could reuse those same types when creating
// packages for commands to send to the drone. We could use reflect
// to loop over all the struct fields, translate it to []byte values
// in little endian (for all except string values which are big endian).
//
// We will need encode methods made by the parser,
// and also a converLittleEndianToBytes function to
// use in the encoding.

//   Pcmd will hold the current state of the piloting commands.
//  type Pcmd struct {
//  	// Boolean flag: 1 if the roll and pitch values should be taken in consideration. 0 otherwise
//  	Flag uint8
//  	// Roll, Left/Right tilt, (Nose rotate around its it's own axis left/right) -100/100
//  	Roll int8
//  	// Front/Back tilt, (Nose up or down) -100/100
//  	Pitch int8
//  	// Horizontal rotation left or right around the up and down axis, -100/100
//  	Yaw int8
//  	// Expressed as signed percentage of the max vertical speed setting, in range [-100, 100]
//  	// -100 corresponds to a max vertical speed towards ground
//  	// 100 corresponds to a max vertical speed towards sky
//  	Gaz int8
//  	// Command timestamp in milliseconds (low 24 bits) + command sequence number (high 8 bits) [0;255].
//  	// TODO: This seems to the the value for how long to keep a given piloting command,
//  	// rather control this in a loop ?
//  	// Set the value to 0.0 for now...or something a bit higher. Will have to test this.
//  	TimestampAndSeqNum float32
//  }

// networkUDPPacket
// networkPacket is the main UDP packet read from the network.
// A network packet can contain multiple ARNetworkAL/frames.
type networkUDPPacket struct {
	// The total size of the UDP packet
	size int
	// The actual UDP data
	data []byte
	// Where to start reading. If there is only one ARNetworkAL frame in the
	// UDP packet this value will be 0. If there are more than one frame in
	// the packet the value will be set to the start position of the next
	// frame in the slice.
	framePos int
}

// udpPacketCreator will keep the sequence counter needed
// to keep track of the sequence number used when sending
// udp packets.
// Since the type is uint8 we don't need any logic to put
// it back to 0 when >255, since it jump back to zero when
// max value is reached.
type udpPacketCreator struct {
	// The sequence number used when sending packets
	//
	// Each individual ID has it's
	// own sequence number, so we create a map
	// of all the id's with a value for sequence number
	sequenceNR map[int]uint8
}

// newUdpPacketCreator will return a new udpPacketCreator,
// and set it's correct default values.
func newUdpPacketCreator() *udpPacketCreator {
	return &udpPacketCreator{
		sequenceNR: make(map[int]uint8),
	}
}

func (u *udpPacketCreator) encodePong(data protocolARNetworkAL) networkUDPPacket {

	u.sequenceNR[int(data.targetBufferID)]++

	pdataType := uint8(2)
	ptargetBufferID := uint8(data.targetBufferID)
	psequenceNR := uint8(u.sequenceNR[int(ptargetBufferID)])
	psize := []byte{8, 0, 0, 0}
	pdata := data.dataARNetwork

	u.sequenceNR[int(ptargetBufferID)]++

	d := []byte{pdataType, ptargetBufferID, psequenceNR}
	d = append(d, psize...)
	d = append(d, pdata...)

	return networkUDPPacket{
		data: d,
	}

}

// encodeAck will prepare and create the UDP ack package that
// is needed is needed to send from the controller for ACK
// packages from the drone.
func (u *udpPacketCreator) encodeAck(targetBufferID int, sequenceNR uint8) networkUDPPacket {
	// To acknowledge data, simply send back a frame with the Ack data type,
	// a buffer ID of 128+Data_Buffer_ID, and the data sequence number as the
	// data.
	// E.g. : To acknowledge the frame    "(hex) 04 0b 42 0b000000 12345678",
	// you will need to send a frame like "(hex) 01 8b 01 08000000 42"

	pdataType := uint8(1)
	ptargetBufferID := uint8(targetBufferID + 128)
	psequenceNR := sequenceNR
	// Ack is always 8 bytes. 7 bytes of header, and 1 byte for the received
	// sequence number put into the data part.
	psize := []byte{8, 0, 0, 0}
	// Put the received sequence number into the data payload
	pdata := uint8(sequenceNR)

	u.sequenceNR[int(ptargetBufferID)]++

	d := []byte{pdataType, ptargetBufferID, psequenceNR}
	d = append(d, psize...)
	d = append(d, pdata)

	return networkUDPPacket{
		data: d,
	}
}

// encodeCmd will encode and prepare the Command package to be sent over UDP.
func (u *udpPacketCreator) encodeCmd(c Command, argument Encoder) networkUDPPacket {
	// Data types:
	// The ARNetworkAL library supports 4 types of data:
	//  • Ack(1): Acknowledgment of previously received data
	//  • Data(2): Normal data (no ack requested)
	//  • Low latency data(3): Treated as normal data on the network, but are
	//    given higher priority internally
	//  • Data with ack(4): Data requesting an ack. The receiver must send an
	//    ack for this data !

	// Controller To Device buffers
	// • Non ack data (periodic commands for piloting and camera orientation).
	//   Non ack data (periodic commands for piloting and camera orientation).
	//   This buffers transports ARCommands.
	//   {
	//   .ID = 10
	//   .dataType = ARNETWORKAL FRAME TYPE DATA;
	//   ...
	//   }
	//
	// • Ack data (Events, settings ...).
	//   Ack data (Events, settings ...).
	//   This buffers transports ARCommands.
	//   {
	//   .ID = 11
	//   .dataType = ARNETWORKAL FRAME TYPE DATA WITH ACK;
	//   ...
	//   }
	//
	// • Emergency data (Emergency command only).
	//   This buffers transports ARCommands.
	//   {
	//   .ID = 12
	//   .dataType = ARNETWORKAL FRAME TYPE DATA WITH ACK;
	//   ...
	//   }
	//
	// • ARStream video acks.
	//   This buffers transports ARStream data.
	//   {
	//   .ID = 13
	//   .dataType = ARNETWORKAL FRAME TYPE DATA LOW LATENCY;
	//   ...
	//   }

	// Setting buffer to 10 which is no-ack for ARCommands
	// 11 is for packages that should be ack'ed.
	const buffer int = 10

	// setting type to data no-ack
	pdataType := uint8(2)
	// ARCommands uses buffer 11 ?
	ptargetBufferID := uint8(buffer)

	u.sequenceNR[buffer]++
	psequenceNR := u.sequenceNR[buffer]
	// Convert the content of the Command from input argument from struct to []byte
	pdata := convertCMDToBytes(Command(c))

	adata := argument.Encode()
	log.Printf("%#v\n", adata)

	// The header size is 7 bytes, 1+1+1+4.
	const headerSize uint32 = 7

	// Get the size, and convert it to a []byte with length of 4.
	size := uint32(len(pdata)) + uint32(len(adata)) + headerSize
	var buf bytes.Buffer
	err := binary.Write(&buf, binary.LittleEndian, size)
	if err != nil {
		fmt.Printf("error: binary write failed: %v\r\n", err)
	}
	psize := buf.Bytes()

	// Create the data package by putting the values in the correct places.
	d := []byte{pdataType, ptargetBufferID, psequenceNR}
	d = append(d, psize...)
	d = append(d, pdata...)
	d = append(d, adata...)

	return networkUDPPacket{
		data: d,
	}
}

func convertCMDToBytes(c Command) []byte {

	var buf bytes.Buffer

	rv := reflect.ValueOf(c)

	for i := 0; i < rv.NumField(); i++ {
		f := rv.Field(i)
		v := (*value)(unsafe.Pointer(&f))
		v.flag &^= flagRO
		binary.Write(&buf, binary.LittleEndian, f.Interface())
	}

	return buf.Bytes()

}

type value struct {
	_    unsafe.Pointer
	_    unsafe.Pointer
	flag flag
}

type flag uintptr

const (
	flagStickyRO flag = 1 << 5
	flagEmbedRO  flag = 1 << 6
	flagRO       flag = flagStickyRO | flagEmbedRO
)

// decode will decode a whole UDP packet given as input,
// and return a frame of the ARNetworkAL protocol, it will return error==
// io.EOF when decoding of the whole packet is done.
// If the there are more than one ARNetworkAL frame in the UDP packet the
// method will return error == nil, and the method should be run over again
// until io.EOF is received.
func (packet *networkUDPPacket) decode() (protocolARNetworkAL, error) {
	// TODO: Make the program check that the length of the packet is the
	// same as the size field, and if they are not equal do something
	// about it.......check if this verification is needed at all, or
	// if is already handled in the ARNetworkAL protocol itself ?
	frame := protocolARNetworkAL{
		dataType:       int(packet.data[packet.framePos+0]),
		targetBufferID: int(packet.data[packet.framePos+1]),
		sequenceNR:     int(packet.data[packet.framePos+2]),
		dataARNetwork:  []byte{},
	}

	fmt.Printf("* Content of frame : protocolARNetworkAL%+v\r\n", frame)

	// Get the size of the ARNetworkAL frame. Size includes the header of 7bytes.
	var size uint32
	ConvLittleEndianSliceToNumeric(packet.data[packet.framePos+3:packet.framePos+7], &size)

	frame.size = int(size)
	frame.dataARNetwork = packet.data[packet.framePos+7 : packet.framePos+frame.size]

	// Figure out if there are another frame after this one.
	// This can be checked if there are a complete header
	// of 7bytes following directly afte the current frame.
	const headerSize = 7

	if packet.framePos+frame.size+headerSize <= packet.size {
		packet.framePos = packet.framePos + frame.size

		return frame, nil

	}

	return frame, io.EOF
}

// networkFrame
// A network frame (ARNetworkAL)looks like this, and in the following order :
// - dataType 1 byte,
// - targetBufferID 1 byte,
// - sequeneNumber 1 Byte,
// - frameSize 4 Bytes (little endian) for the whole ARNetworkAL frame including 7bit header,
// - data n bytes (this is the actual drone data ARNetwork),
//
//	Example of size:
//	01 ba 27 08000000 42, 02 0b c3 0b000000 12345678
//  --size 0x08=8byte---, --size 0x0b=11byte--------
type protocolARNetworkAL struct {
	//
	// Data types
	// • Ack(1): Acknowledgment of previously received data
	//   To Ack a frame, set type to 1,
	//   add 128 to the value of the bufferID of the package that requires Ack,
	//	 new unique sequence nr. for the ack buffer,
	// • Data(2): Normal data (no ack requested)
	// • Low latency data(3): Treated as normal data on the network, but are
	//   given higher priority internally
	// • Data with ack(4): Data requesting an ack. The receiver must send an
	//   ack for this data !
	dataType int
	//
	// • [0; 9]: Reserved values for ARNetwork internal use.
	// • [10; 127]: Data buffers.
	// • [128; 255]: Acknowledge buffers.
	targetBufferID int
	sequenceNR     int
	size           int
	dataARNetwork  []byte
}

// decode will try to decode the command found in the ARNetworkAL frame,
// if it fails it will return an empty protocolARCommands struct, and the
// error
func (p *protocolARNetworkAL) decode() (cmd protocolARCommands, cmdArgs interface{}, err error) {
	const headerSize = 7

	// Start preparing a cmd struct that will be returned to the caller.
	cmd = protocolARCommands{
		project: int(p.dataARNetwork[0]),
		class:   int(p.dataARNetwork[1]),
		size:    p.size - headerSize,
	}

	//fmt.Println("1. inside command contains = ", cmd)

	// Since we read and slice out 2 bytes, we need to use an uint16 to
	// write into. We then convert the uint16 to int, and store the
	// value in the command field of the struct.
	var tmpCommand uint16
	ConvLittleEndianSliceToNumeric(p.dataARNetwork[2:4], &tmpCommand)
	cmd.command = int(tmpCommand)

	//fmt.Printf("tmpCommand = %v, %T\n", tmpCommand, tmpCommand)
	//fmt.Println("2. inside command contains = ", cmd)
	//fmt.Println("******************Parsing of command*************************")
	//fmt.Printf("* cmd.project = %v\n", cmd.project)
	//fmt.Printf("* cmd.class = %v\n", cmd.class)
	//fmt.Printf("* cmd.command = %v\n", cmd.command)
	//fmt.Printf("* cmd.size = %v\n", cmd.size)
	//fmt.Println()

	// #### Done parsing the project/class/cmd
	// ------------------- Figure out arguments and types from here.

	//... testing from here
	// Creating a temporary command value of type command (which is the same as used in the map)
	// of project/class/def with the values parsed earlier, we need this type to compare against
	// the map.
	// The key's of map are a variable of type 'command', and we will check if we find that
	// same variable later.
	c := Command{
		Project: ProjectDef(cmd.project),
		Class:   ClassDef(cmd.class),
		Cmd:     CmdDef(cmd.command),
	}
	//fmt.Printf("c = %#v\n", c)

	// prereq : Parse arg struct, and create arg map which maps arg struct to cmd.
	arguments := p.dataARNetwork[4:cmd.size]
	//fmt.Printf("--- arguments = %+v\n", arguments)
	//fmt.Println("******************End Parsing of command*********************")

	// To get the actual type we have to check the map holding all the commands, to get it's
	// actual type.
	// Check if the command c with the correct values are specified in the map, and if it is...
	v, ok := CommandMap[c]
	if ok {
		//fmt.Printf("+++++ main : Content before calling decode of v = %+v, arguments = %v\n", v, arguments)

		//-- !!!!!!!!! If you are running the _test file uncomment the line below
		// and comment out the 2 lines below that one so the output doesn't get flooded.
		//_ = v.decode(arguments)
		cmdArgs = v.Decode(arguments)
		// fmt.Printf("cmdargmain : type %T, arguments = %+v\n", cmdArgs, cmdArgs)

		// Check the type...for testing
		//_, ok := args.(ardrone3PilotingStateAttitudeChangedArguments)
		//fmt.Println("The result of the type check for arguments = ", ok)

	}

	return cmd, cmdArgs, nil
}

// • Project or Feature ID (1 byte)
// • Class ID in the project/feature (1 byte)
// • Command ID in the class (2 bytes)
type protocolARCommands struct {
	project int
	class   int
	command int
	// size is included since we have now stripped off the header of 7 bytes,
	// the size is for project+class+command+arguments, which again is the
	// same size as the whole frame minus the header size of 7 bytes.
	size int
}

func (d *Drone) start() {
	for {
		var err error

		// Since we need to use individual sequence number counters for each
		// buffer a udpPacketCreator will keep track of them, and increment
		// the currect buffer sequence number when a new package are created.
		// All UDP packet encoding methods are tied to this type.
		packetCreator := newUdpPacketCreator()

		ctxBg := context.Background()
		ctx, cancel := context.WithCancel(ctxBg)

		// Will handle all the events generated by input actions from keyboard etc.
		go d.handleInputAction(*packetCreator, ctx)

		// Check for keyboard press, and generate appropriate inputActions's.
		go d.readKeyBoardEvent(ctx)

		// Initialize the network connection to the drone.
		// If the connection fails retry 20 times before giving up.
		//
		// TODO:
		// Make it call return-home if unable to initialize.
		log.Println("Initializing the traffic with the drone, and starting controller UDP listener.")
		for i := 0; i < 20; i++ {
			err := d.Discover()
			if err != nil {
				log.Printf("error: client Discover failed: %v\n", err)
				time.Sleep(time.Second * 2)
				continue
			}

			break
		}

		// create an 'empty' UDP listener.
		d.connUDPRead, err = net.ListenPacket("udp", ":"+d.portD2C)
		if err != nil {
			log.Println("error: failed to start listener", err)
		}

		// Start the reading of whole UDP packets from the network,
		// and put them on the Drone.chReceivedUDPPacket channel.
		go d.readNetworkUDPPacketsD2C(ctx)

		// Prepare and dial the UDP connection from controller to drone.
		udpAddr, err := net.ResolveUDPAddr("udp", d.addressDrone+":"+d.portC2D)
		if err != nil {
			log.Printf("error: failed to resolveUDPAddr: %v", err)
		}
		d.connUDPWrite, err = net.DialUDP("udp", nil, udpAddr)
		if err != nil {
			log.Printf("error: failed to DialUDP: %v", err)
		}

		// Start the scheduler which will make sure that if there are
		// Pcmd packets to be sent, they are only sent at a fixed 50
		// milli second interval.
		go d.PcmdPacketScheduler(ctx)

		// Start the sender of UDP packets,
		// will send UDP packets received at the Drone.chSendingUDPPacket
		// channel.
		go d.writeNetworkUDPPacketsC2D(ctx)

		go d.handleReadPackages(packetCreator, ctx)

		// Wait here until receiving on quit channel. Trigger by pressing
		// 'q' on the keyboard.
		select {
		case <-d.chQuit:
			cancel()
			return
		case <-d.chNetworkConnect:
			cancel()
			time.Sleep(time.Second * 3)
			continue
		}
	}
}

func main() {
	drone := NewDrone()

	drone.start()
}
