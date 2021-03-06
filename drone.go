// The latest version of the ardrone3.xml document can be found at
// https://github.com/Parrot-Developers/arsdk-xml/tree/master/xml

package parrotbebop

import (
	"context"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"
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
	// gps Data
	gps GPS
	// moveToBuffer is a FIFO buffer for storing the gps positions
	// of the route to fly.
	moveToBuffer *moveToBuffer
}

// TODO:
// We can send moveTo messages telling the location to move to with the
// Ardrone3Pilotingmoveto command.
// We can check when that request is fullfilled by checking for a message
// of type Ardrone3PilotingStatemoveToChanged. Maybe need timeout for this ??
// We can then issue the next command.
// For now it seems like we will need a buffer for the moveTo commands, so it
// will pick the next when the previous is done.
// Pressing space should add the next moveTo command to the buffer.
// moveTo paths should be able to be read from file, or other API ? Geofencing ?

// NewDrone will initalize all the variables needed for a drone,
// like ports used, ip adresses, etc.
func NewDrone() *Drone {
	d := &Drone{
		addressDrone: "192.168.42.1",
		portDiscover: "44444",
		//portC2D:        "54321", // This one is now assigned via discovery
		portD2C:        "43210",
		portRTPStream:  "55004",
		portRTPControl: "55005",

		chReceivedUDPPacket:   make(chan networkUDPPacket),
		chSendingUDPPacket:    make(chan networkUDPPacket),
		chInputActions:        make(chan inputAction),
		chQuit:                make(chan struct{}),
		chNetworkConnect:      make(chan struct{}),
		chPcmdPacketScheduler: make(chan networkUDPPacket),

		pcmd: Ardrone3PilotingPCMDArguments{
			Flag:               0,
			Roll:               0,
			Pitch:              0,
			Yaw:                0,
			Gaz:                0,
			TimestampAndSeqNum: 0,
		},

		// The default gps values received from the drone when not
		// connected is 500. We set all the values 500 and check
		// later in the code for that value, so we for example don't
		// initiate a moveTo when there is no connection, or add some
		// lat/lon distance if the current register value are 500.
		gps: GPS{
			chCurrentLocation: make(chan gpsLatLonAlt),
			connected:         false,
			latitude:          500,
			longitude:         500,
			altitude:          500,
			latitudeMoveTo:    500,
			longitudeMoveTo:   500,
			altitudeMoveto:    500,
		},

		moveToBuffer: newMoveToHandler(),
	}

	go func() {
		<-d.chQuit
		log.Printf("Operator asked to stop driver.\n")
		os.Exit(0)
	}()

	return d
}

// -----------------------------GPS Related---------------------------------------

// gpsLatLongAlt is used for messaging position data between
// go routines.
type gpsLatLonAlt struct {
	latitude float64
	// Longitude East/West
	longitude float64
	// Altitude height in meters above sea level
	altitude float64
}

// GPS will hold all the current values of the current
// gps location, and also the coordinate to move to
// next if moveTo action have been issued.
type GPS struct {
	chCurrentLocation chan gpsLatLonAlt
	// connected ?
	connected bool
	// latitude North/South
	latitude float64
	// Longitude East/West
	longitude float64
	// Altitude height in meters above sea level
	altitude float64

	// latitude North/South
	latitudeMoveTo float64
	// Longitude East/West
	longitudeMoveTo float64
	// Altitude height in meters above sea level
	altitudeMoveto float64
	// Are the drone currently in a moveTo action ?
	// This value should be set to true when a moveTo are started,
	// and it should be set to false when a message from the drone
	// of type Ardrone3PilotingStatemoveToChanged are received.
	doingMoveTo bool
	// Initiate an execution of a moveTo to the next position in buffer.
	chMoveToExecute chan struct{}
	// Cancel the execution of a moveTo command
	chMoveToCancel chan struct{}
	// When a moveTo command is succesful a Ardrone3PilotingStatePositionChanged
	// command is sent from the drone. In the actionsD2C we will check
	// for such commands and send a signal here, so we know that we
	// can pull the next waypoint.
	chMoveToPositionDone chan struct{}
}

// StartHandling, start handling incomming gps packages, and fill
// the registers with the current location values.
func (g *GPS) StartReadingPosition() {
	for v := range g.chCurrentLocation {
		if v.latitude == 500 || v.longitude == 500 || v.altitude == 500 {
			g.connected = false
		}
		g.latitude = v.latitude
		g.longitude = v.longitude
		g.altitude = v.altitude

		log.Printf("gps location data: %#v\n", g)
	}
}

// startMoveToExecutor
// The plan here is to receive a signal for when to execute a
// moveTo command to the drone, or to cancel it.
//
// When a moveto signal is reveived we will pull one waypoint
// at a time from the moveTo buffer, but before pulling a new
// waypoint we will wait for a positiosChanged command from
// the drone, since that will indicate that the last moveTo
// command was executed and done by the drone, and we can pull
// a new value and send another  moveTo package to the drone.
//
// When a cancel signal is received we should immediately send
// a moveTo cancel package to the drone, and also stop any moveTo
// processes.
func (d *Drone) startMoveToExecutor(packetCreator *udpPacketCreator, ctx context.Context) {
	for {
		<-d.gps.chMoveToExecute
		ctx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		wg.Add(1)

		go func(ctx context.Context) {
			for {

				ticker := time.NewTicker(time.Second * 5)

				select {
				case <-ctx.Done():
					return
				case <-d.gps.chMoveToCancel:
					p := packetCreator.encodeCmd(Command(PilotingCancelMoveTo), &Ardrone3PilotingCancelMoveToArguments{})
					d.chSendingUDPPacket <- p
					wg.Done()
				case wp := <-d.moveToBuffer.chNewWayPointOut:
					// Get a new wp, create the argument, and send the udp packet.
					arg := &Ardrone3PilotingmoveToArguments{
						Latitude:  wp.latitude,
						Longitude: wp.longitude,
						Altitude:  wp.altitude,
					}

					p := packetCreator.encodeCmd(Command(PilotingmoveTo), arg)
					d.chSendingUDPPacket <- p

					// Check if the waypoint was reached, and we got a confirmation
					// from the drone. If a waypoint is not received we break out,
					// loop and pick a new waypoint.
					select {
					case <-d.gps.chMoveToPositionDone:
						log.Printf("moveToPositionDone received, breaking out and looping")
						break
					case <-ticker.C:
						log.Printf("moveToPositionDone not received, ticker occured, looping")
						break
					}

				}
			}
		}(ctx)

		wg.Wait()
		cancel()

	}

	// for {
	// 	wp, err := d.moveToBuffer.pullWayPointNext()
	// 	if err != nil {
	// 		log.Printf("info: moveToBufferEmpty, breaking out\n")
	// 		break
	// 	}
	//
	// 	arg := &Ardrone3PilotingmoveToArguments{
	// 		Latitude:  wp.latitude,
	// 		Longitude: wp.longitude,
	// 		Altitude:  wp.altitude,
	// 	}
	//
	// 	p := packetCreator.encodeCmd(Command(PilotingmoveTo), arg)
	// 	d.chSendingUDPPacket <- p
	// }

	//------------------------------------------

	// for {
	// 	select {
	// 	case <-d.gps.chMoveToCancel:
	//
	// 		p := packetCreator.encodeCmd(Command(PilotingCancelMoveTo), &// Ardrone3PilotingCancelMoveToArguments{})
	// 		d.chSendingUDPPacket <- p
	// 		log.Printf("*************************************************************\n")
	// 		log.Printf("startMoveToExecutor: chMoveToCancel received\n")
	// 		log.Printf("*************************************************************\n")
	// 	case <-d.gps.chMoveToExecute:
	// 		// TODO:
	// 		log.Printf("*************************************************************\n")
	// 		log.Printf("startMoveToExecutor: chMoveToExecute received\n")
	// 		log.Printf("*************************************************************\n")
	// 	}
	// }
}

// --------------------------------------------------------------------

// moveToBuffer holds the buffer of all the waypoints
// and the logic to receive, push and pull waypoints.
type moveToBuffer struct {
	// all the waypoints registered
	waypoints        []gpsLatLonAlt
	chNewWayPointIn  chan gpsLatLonAlt
	chNewWayPointOut chan gpsLatLonAlt
}

// newmoveToBuffer is a push/pop storage for values.
func newMoveToHandler() *moveToBuffer {
	b := moveToBuffer{
		chNewWayPointIn: make(chan gpsLatLonAlt),
	}

	// Start the moveToBuffer listener, which basically will start
	// listening on the channel for moveTo messages, and add them
	// to the moveTo buffer
	go b.startWayPointReceiver()

	go func() {
		for {
			wp, err := b.pullWayPointNext()
			if err != nil {
				log.Printf("info: no way point in buffer, waiting 1 sec, and continue\n")
				time.Sleep(time.Second * 1)
				continue
			}

			// TODO: Might need to add a select with default here
			// incase the channel is not listening
			// or..maybe not since that would cause the wp to be dropped.
			// Need to check this out.
			b.chNewWayPointOut <- wp
		}
	}()

	return &b
}

// startWayPointReceiver will check if the wp received
// are within the allowed limits. If OK put it on the
// waypoint buffer, if not we just discard the value
// and wait for the next one.
func (s *moveToBuffer) startWayPointReceiver() {
	for {
		wp := <-s.chNewWayPointIn
		// Check if the values are to big, which means no GPS connection
		// where available for calculation, and drop the data if it is
		// an not allowed value
		switch {
		case wp.latitude > 91 || wp.latitude < -91:
			log.Printf("moveToBuffer: not allowed value received: %v\n", wp)
			continue
		case wp.longitude > 181 || wp.longitude < -181:
			log.Printf("moveToBuffer: not allowed value received: %v\n", wp)
			continue
		}
		s.pushWayPointNew(wp)
	}
}

// push will add another item to the end of the buffer with a normal append
func (s *moveToBuffer) pushWayPointNew(d gpsLatLonAlt) {
	s.waypoints = append(s.waypoints, d)
}

// pop will remove and return the first element of the buffer,
// and will return io.EOF if buffer is empty.
func (s *moveToBuffer) pullWayPointNext() (gpsLatLonAlt, error) {
	if len(s.waypoints) == 0 {
		return gpsLatLonAlt{}, io.EOF
	}

	v := s.waypoints[0]
	s.waypoints = append(s.waypoints[0:0], s.waypoints[1:]...)

	return v, nil
}

func (d *Drone) Start() {
	// Check for keyboard press, and generate appropriate inputActions's.
	go d.readKeyBoardEvent()

	// Start handling incomming gps packages, and fill the registers with
	// the current location values.
	go d.gps.StartReadingPosition()

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

		go d.startMoveToExecutor(packetCreator, ctx)

		// Wait here until receiving on quit channel. Trigger by pressing
		// 'q' on the keyboard.
		<-d.chNetworkConnect
		cancel()
		time.Sleep(time.Second * 3)
		continue

	}
}
