// distributor.go - Game of Life simulation distributor.

package gol

import (
	"fmt"
	"sync"
	"time"

	"uk.ac.bris.cs/gameoflife/util"
)

// Constants
const (
	// Size of halo used to surround compute region
	DefaultHaloOffset = 1
	// Initial delay before reporter starts sending events
	InitialDelay = 2 * time.Second
)

type distributorChannels struct {
	events     chan<- Event
	ioCommand  chan<- ioCommand
	ioIdle     <-chan bool
	ioFilename chan<- string
	ioOutput   chan<- uint8
	ioInput    <-chan uint8
	keyPresses <-chan rune
}

type (
	// Cell represents a single cell in the game grid.
	Cell struct {
		X     int
		Y     int
		Alive bool
	}

	// Region is a sub-section of the Field used for parallel updates
	Region struct {
		Field  [][]Cell
		Start  int
		End    int
		Height int
		Width  int
	}

	// Field represents a game grid.
	Field struct {
		Data   [][]Cell
		Height int
		Width  int
	}

	// World contains the main simulation state
	World struct {
		Field   Field
		Height  int
		Width   int
		Threads int // Number of threads to use
	}
)

type (
	// BrokerState tracks simulation statistics
	BrokerState struct {
		turns int
		cells int
	}

	// Broker manages the state of the simulation and provides access to it.
	Broker struct {
		State    BrokerState
		GetTurns chan int
		IncTurn  chan struct{}
		GetCells chan int
		SetCells chan int
		Stop     chan bool
	}
)

// Reporter periodically reports the state of the simulation.
type Reporter struct {
	EventsCh       chan<- Event
	Broker         *Broker
	ReportInterval time.Duration
	Stop           chan bool
}

// Initializes a new field with the given height and width
// (associated with the Field struct).
func (field *Field) cultivate(height, width int) Field {
	land := make([][]Cell, height)
	for i := range land {
		land[i] = make([]Cell, width)
	}
	field.Data = land
	return *field
}

// Applies the Game of Life rules to the cells within the specified region.
// This function iterates through each cell, checks its neighbors, and updates the cell's state accordingly based on
// Conway's Game of Life rules. The updated cells are sent to regionCh, and flipped cells are sent to flippedCh.
// (associated with the Region struct).
func (region *Region) update(regionCh chan<- [][]Cell, flippedCh chan<- []util.Cell) {
	field := Field{
		Height: region.Height,
		Width:  region.Width,
	}

	field.cultivate(region.Height, region.Width)

	// Initialize an empty list to keep track of cells that change state (flip) during the update.
	flipped := []util.Cell{}

	// Iterate through each cell in the region
	for y := DefaultHaloOffset; y < region.Height+DefaultHaloOffset; y++ {
		for x := 0; x < region.Width; x++ {
			currentCell := region.Field[y][x]
			nextCell := currentCell
			aliveNeighbours := 0

			// Count the number of alive neighbors for the current cell
			for i := -1; i <= 1; i++ {
				for j := -1; j <= 1; j++ {
					wx := x + i
					wy := y + j
					wx += region.Width
					wx %= region.Width
					if (j != 0 || i != 0) && region.Field[wy][wx].Alive {
						aliveNeighbours++
					}
				}
			}

			// Apply Conway's Game of Life rules
			if (aliveNeighbours < 2) || (aliveNeighbours > 3) {
				nextCell.Alive = false
			}
			if aliveNeighbours == 3 {
				nextCell.Alive = true
			}

			// Track flipped cells
			if currentCell != nextCell {
				flipped = append(flipped, util.Cell{
					X: x,
					Y: y - DefaultHaloOffset + region.Start,
				})
			}

			// Update the cell in the region
			field.Data[y-DefaultHaloOffset][x] = nextCell
		}
	}

	// Send the updated region to the channel
	regionCh <- field.Data

	// Send the flipped cells to the channel
	flippedCh <- flipped
}

// Initializes the world by reading input and updating the initial state.
// It reads input values from the ioInput channel and populates the world's Field
// based on the values received. If a cell is initially alive, it triggers a CellFlipped
// event and adds the cell to the flipped list. The flipped cells are sent to the events channel.
// (associated with the World struct).
func (world *World) populate(c distributorChannels) {

	// Iterate through each row and column to populate the world's Field
	for y := 0; y < world.Height; y++ {
		for x := 0; x < world.Width; x++ {
			cell := <-c.ioInput
			world.Field.Data[y][x] = Cell{X: x, Y: y, Alive: cell == 255}

			// Trigger CellFlipped event for initially alive cells
			if cell == 255 {
				c.events <- CellFlipped{0, util.Cell{X: x, Y: y}}
			}
		}
	}
}

// Calculates and returns the region assigned to a specific worker.
// The region represents a portion of the world that a worker is responsible for processing
// in parallel during the Game of Life simulation. The function ensures that each worker
// gets a fair share of rows from the world, considering the specified number of threads.
// (associated with the World struct).
func (world *World) region(w int) Region {
	field := Field{
		Height: 0,
		Width:  0,
	}
	regionHeight := world.Height / world.Threads
	start := w * regionHeight
	end := (w + 1) * regionHeight

	// Adjust the end row for the last worker to ensure coverage of all rows.
	if w == world.Threads-1 {
		end = world.Height
	}
	regionHeight = end - start

	// Determine the pointers to the rows above and below the region.
	downRowPtr := end % world.Height
	upRowPtr := (start - 1 + world.Height) % world.Height

	field.Data = make([][]Cell, regionHeight+2)

	// Include the row above the region as the last row of the region's data.
	field.Data[0] = world.Field.Data[upRowPtr]

	// Populate the region's data with rows from the world.
	for row := 1; row <= regionHeight; row++ {
		field.Data[row] = world.Field.Data[start+row-1]
	}

	// Include the row below the region as the last row of the region's data.
	field.Data[regionHeight+1] = world.Field.Data[downRowPtr]

	return Region{
		Field:  field.Data,
		Start:  start,
		End:    end,
		Height: regionHeight,
		Width:  world.Width,
	}
}

// Applies the rules of Conway's Game of Life to the entire world for a given turn.
// The function coordinates the update process, dividing the world into regions handled
// by individual worker goroutines. The updated state is communicated through channels,
// (associated with the World struct).
func (world *World) update(turn int, c distributorChannels) {

	// Initialize variables to store the updated state of each region and flipped cells.
	var newFieldData [][]Cell
	var newFlippedData []util.Cell

	// Create channels to receive updates from individual worker goroutines.
	regionChannel := make([]chan [][]Cell, world.Threads)
	flippedChannel := make([]chan []util.Cell, world.Threads)

	// Use WaitGroup to wait for all worker goroutines to complete their updates.
	var wg sync.WaitGroup
	wg.Add(world.Threads)

	// Spawn worker goroutines for each region.
	for workerID := 0; workerID < world.Threads; workerID++ {
		regionChannel[workerID] = make(chan [][]Cell)
		flippedChannel[workerID] = make(chan []util.Cell)

		// Get the region assigned to the worker.
		region := world.region(workerID)

		// Launch a worker goroutine to update the assigned region.
		go func(workerID int) {
			defer func() {
				// Close channels to signal completion to the main loop.
				close(regionChannel[workerID])
				close(flippedChannel[workerID])

				// Decrement the WaitGroup to signal completion of the goroutine.
				wg.Done()
			}()
			// Call the update method for the specific region.
			region.update(regionChannel[workerID], flippedChannel[workerID])
		}(workerID)
	}

	// Collect the updated state from each worker goroutine.
	for w := 0; w < world.Threads; w++ {
		region := <-regionChannel[w]
		newFieldData = append(newFieldData, region...)
		flipped := <-flippedChannel[w]
		newFlippedData = append(newFlippedData, flipped...)
	}

	// Report flipped cells to the events channel.
	for f := range newFlippedData {
		c.events <- CellFlipped{
			CompletedTurns: turn,
			Cell:           newFlippedData[f],
		}
	}

	// Update the overall state of the world.
	world.Field.Data = newFieldData
}

// save saves the current state of the world to a file, representing the visual output
// of the Game of Life simulation at a specific turn
// (associated with the World struct).
func (world *World) save(turn int, c distributorChannels) {
	filename := fmt.Sprintf("%vx%vx%v", world.Width, world.Width, turn)

	c.ioCommand <- ioOutput
	c.ioFilename <- filename

	// Iterate through each cell in the world's field and convert its alive state into a binary value.
	for y := 0; y < world.Height; y++ {
		for x := 0; x < world.Width; x++ {
			var aliveValue uint8
			if world.Field.Data[y][x].Alive {
				aliveValue = 255
			}
			// Send the binary values to the distributor's ioOutput channel for output.
			c.ioOutput <- aliveValue
		}
	}

	c.events <- ImageOutputComplete{
		CompletedTurns: turn,
		Filename:       filename,
	}
}

// Initializes and starts the Broker, which is responsible for managing the simulation state.
// The Broker communicates with other components using channels, handling requests to retrieve and update state.
// It runs an infinite loop, waiting for various channel operations and responding accordingly.
// (associated with the Broker struct).
func (broker *Broker) start() {
	for {
		select {

		// Sends the current turn count to the GetTurns channel upon request.
		case broker.GetTurns <- broker.State.turns:

		// Increments the turn count upon receiving the IncTurn signal.
		case <-broker.IncTurn:
			broker.State.turns++

		// Sends the current cell count to the GetCells channel upon request.
		case broker.GetCells <- broker.State.cells:

			// Updates the cell count when receiving a new value through the SetCells channel.
		case broker.State.cells = <-broker.SetCells:

			// Exits the loop and terminates the Broker when receiving the Stop signal.
		case <-broker.Stop:
			return
		}
	}
}

// Initializes and starts the Reporter, which is responsible for periodically reporting
// the simulation state. The Reporter interacts with the Broker to gather information about
// completed turns and the current cell count, and it reports this information through the
// EventsCh channel at regular intervals.
// (associated with the Reporter struct).
func (reporter *Reporter) start() {

	initialDelay := time.After(InitialDelay)

	// Create a ticker to trigger periodic reporting based on the specified ReportInterval.
	ticker := time.NewTicker(reporter.ReportInterval)

	// Ensure the ticker is stopped when the function exits.
	defer ticker.Stop()

	for {
		select {

		// Initial delay elapsed, start reporting.
		case <-initialDelay:

		// Ticker triggers periodic reporting based on ReportInterval.
		case <-ticker.C:
			turns := <-reporter.Broker.GetTurns
			cellsCount := <-reporter.Broker.GetCells
			reporter.EventsCh <- AliveCellsCount{
				CompletedTurns: turns,
				CellsCount:     cellsCount,
			}

		// Stop signal received, exit the loop and terminate the Reporter.
		case <-reporter.Stop:
			return
		}
	}
}

// Calculates and returns a slice containing the coordinates of all alive cells in the world.
// The function iterates through each cell in the world's field and collects the coordinates of
// cells that are currently alive.
// (associated with the World struct).
func (world *World) alive() []util.Cell {
	var alive []util.Cell

	// Iterate through each row in the world's field.
	for y, row := range world.Field.Data {
		var aliveCellsInRow []util.Cell

		// Iterate through each cell in the current row.
		for x, cell := range row {
			if cell.Alive {
				alive = append(alive, util.Cell{X: x, Y: y})
			}
		}
		alive = append(alive, aliveCellsInRow...)
	}

	// Return the final slice containing the coordinates of all alive cells.
	return alive
}

// initializeWorld creates and initializes the world with the specified parameters, then populates it with input data.
func initializeWorld(p Params, c distributorChannels) World {

	// Initialize the field with the specified dimensions and cultivate the land.
	field := Field{
		Height: p.ImageHeight,
		Width:  p.ImageWidth,
	}
	field.cultivate(p.ImageHeight, p.ImageWidth)

	// Create and initialize the world with the specified parameters,
	// then populate it with input data.
	world := World{
		Field:   field,
		Height:  p.ImageHeight,
		Width:   p.ImageWidth,
		Threads: p.Threads,
	}
	world.populate(c)

	return world
}

// startBroker creates and initializes the broker, responsible for managing the simulation state.
// It also starts the broker in a separate goroutine.
func startBroker(world *World, c distributorChannels) *Broker {
	broker := Broker{
		GetTurns: make(chan int),
		IncTurn:  make(chan struct{}),
		GetCells: make(chan int),
		SetCells: make(chan int),
		Stop:     make(chan bool),
	}

	go broker.start()

	return &broker
}

// startReporter creates and initializes the reporter, responsible for periodic state reporting.
// It also starts the reporter in a separate goroutine.
func startReporter(c distributorChannels, broker *Broker) *Reporter {
	reporter := Reporter{
		EventsCh:       c.events,
		Broker:         broker,
		ReportInterval: InitialDelay,
		Stop:           make(chan bool),
	}

	go reporter.start()

	return &reporter
}

// The main entry point for the Game of Life simulation.
// Coordinates the initialization, execution, and termination of the simulation.
func distributor(p Params, c distributorChannels) {

	filename := fmt.Sprintf("%vx%v", p.ImageWidth, p.ImageHeight)

	c.ioCommand <- ioInput
	c.ioFilename <- filename

	world := initializeWorld(p, c)
	broker := startBroker(&world, c)
	reporter := startReporter(c, broker)

	paused := false
	turn := 0

	func() {
		// Main simulation loop.
		for turn < p.Turns {
			select {
			case cmd := <-c.keyPresses:
				switch cmd {
				// Saves the current state of the world to a file.
				case 's':
					world.save(turn, c)
				// Saves the state, sends a Quitting state change event, and exits the loop.
				case 'q':
					world.save(turn, c)
					c.events <- StateChange{
						CompletedTurns: turn,
						NewState:       Quitting,
					}
					return
				// Toggles the paused state and sends a corresponding state change event.
				case 'p':
					paused = !paused
					if paused {
						c.events <- StateChange{
							CompletedTurns: turn,
							NewState:       Paused,
						}
					} else {
						c.events <- StateChange{
							CompletedTurns: turn,
							NewState:       Executing,
						}
					}
				// Resets the paused state.
				default:
					paused = false
				}

				// Check if the key press was 'q' and exit the function if so.
				if cmd == 'q' {
					return
				}
			default:
				if !paused {
					world.update(turn, c)
					broker.SetCells <- len(world.alive())
					broker.IncTurn <- struct{}{}
					c.events <- TurnComplete{
						CompletedTurns: turn,
					}
					turn++
				}
			}
		}
	}()

	reporter.Stop <- true

	completedTurns := <-broker.GetTurns

	broker.Stop <- true

	c.events <- FinalTurnComplete{
		CompletedTurns: completedTurns,
		Alive:          world.alive(),
	}

	world.save(completedTurns, c)

	// Make sure that the Io has finished any output before exiting.
	c.ioCommand <- ioCheckIdle
	<-c.ioIdle

	c.events <- StateChange{
		CompletedTurns: completedTurns,
		NewState:       Quitting,
	}

	// Close the channel to stop the SDL goroutine gracefully. Removing may cause deadlock.
	close(c.events)
}
