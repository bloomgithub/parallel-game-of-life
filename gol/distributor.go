package gol

import (
	"fmt"
	"log"
	"sync"
	"time"

	"uk.ac.bris.cs/gameoflife/util"
)

const (
	DefaultHaloOffset = 1
	InitialDelay      = 2 * time.Second
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

type Cell struct {
	X     int
	Y     int
	Alive bool
}

type Field struct {
	Data   [][]Cell
	Height int
	Width  int
}

func (field *Field) cultivate(height, width int) Field {
	land := make([][]Cell, height)
	for i := range land {
		land[i] = make([]Cell, width)
	}
	field.Data = land
	return *field
}

type Region struct {
	Field  [][]Cell
	Start  int
	End    int
	Height int
	Width  int
}

func (region *Region) update(regionCh chan<- [][]Cell, flippedCh chan<- []util.Cell) {
	field := Field{
		Height: region.Height,
		Width:  region.Width,
	}
	field.cultivate(region.Height, region.Width)

	flipped := []util.Cell{}
	for y := DefaultHaloOffset; y < region.Height+DefaultHaloOffset; y++ {
		for x := 0; x < region.Width; x++ {
			currentCell := region.Field[y][x]
			nextCell := currentCell
			aliveNeighbours := 0
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
			if (aliveNeighbours < 2) || (aliveNeighbours > 3) {
				nextCell.Alive = false
			}
			if aliveNeighbours == 3 {
				nextCell.Alive = true
			}
			if currentCell != nextCell {
				flipped = append(flipped, util.Cell{
					X: x,
					Y: y - DefaultHaloOffset + region.Start,
				})
			}
			field.Data[y-DefaultHaloOffset][x] = nextCell
		}
	}
	regionCh <- field.Data
	flippedCh <- flipped
}

type World struct {
	Field   Field
	Height  int
	Width   int
	Threads int
}

func (world *World) populate(c distributorChannels) {
	flipped := []util.Cell{}
	for y := 0; y < world.Height; y++ {
		for x := 0; x < world.Width; x++ {
			cell := <-c.ioInput
			world.Field.Data[y][x] = Cell{X: x, Y: y, Alive: cell == 255}
			if cell == 255 {
				c.events <- CellFlipped{0, util.Cell{X: x, Y: y}}
				flipped = append(flipped, util.Cell{X: x, Y: y})
			}
		}
	}
	//fmt.Println(len(flipped))
}

func (world *World) region(w int) Region {
	field := Field{
		Height: 0,
		Width:  0,
	}
	regionHeight := world.Height / world.Threads
	start := w * regionHeight
	end := (w + 1) * regionHeight
	if w == world.Threads-1 {
		end = world.Height
	}
	regionHeight = end - start

	downRowPtr := end % world.Height
	upRowPtr := (start - 1 + world.Height) % world.Height

	field.Data = make([][]Cell, regionHeight+2)
	field.Data[0] = world.Field.Data[upRowPtr]
	for row := 1; row <= regionHeight; row++ {
		field.Data[row] = world.Field.Data[start+row-1]
	}
	field.Data[regionHeight+1] = world.Field.Data[downRowPtr]

	return Region{
		Field:  field.Data,
		Start:  start,
		End:    end,
		Height: regionHeight,
		Width:  world.Width,
	}
}

func (world *World) update(turn int, c distributorChannels) {
	var newFieldData [][]Cell
	var newFlippedData []util.Cell

	regionChannel := make([]chan [][]Cell, world.Threads)
	flippedChannel := make([]chan []util.Cell, world.Threads)

	var wg sync.WaitGroup
	wg.Add(world.Threads)

	for workerID := 0; workerID < world.Threads; workerID++ {
		regionChannel[workerID] = make(chan [][]Cell)
		flippedChannel[workerID] = make(chan []util.Cell)
		region := world.region(workerID)
		go func(workerID int) {
			defer func() {
				close(regionChannel[workerID])
				wg.Done()
			}()
			region.update(regionChannel[workerID], flippedChannel[workerID])
		}(workerID)
	}

	for w := 0; w < world.Threads; w++ {
		region := <-regionChannel[w]
		newFieldData = append(newFieldData, region...)
		flipped := <-flippedChannel[w]
		newFlippedData = append(newFlippedData, flipped...)
	}

	for f := range newFlippedData {
		c.events <- CellFlipped{
			CompletedTurns: turn,
			Cell:           newFlippedData[f],
		}
	}

	world.Field.Data = newFieldData
}

func handleKeyPress(cmd rune, paused *bool, turn int, c distributorChannels, world *World) {
	switch cmd {
	case 's':
		world.save(turn, c)
	case 'q':
		world.save(turn, c)
	case 'p':
		togglePause(paused, turn, c)
	default:
		*paused = false
	}
}

func togglePause(paused *bool, turn int, c distributorChannels) {
	*paused = !*paused
	if *paused {
		c.events <- StateChange{
			CompletedTurns: turn,
			NewState:       Paused,
		}
		fmt.Printf("\nCurrent turn: %d\n", turn+1)
	} else {
		c.events <- StateChange{
			CompletedTurns: turn,
			NewState:       Executing,
		}
		fmt.Printf("\nContinuing\n")
	}
}

func evolveToNextGeneration(turn int, c distributorChannels, broker *Broker, world *World) {
	world.update(turn, c)
	broker.SetCells <- len(world.alive())
	broker.IncTurn <- struct{}{}
	c.events <- TurnComplete{
		CompletedTurns: turn,
	}
}

func (world *World) Life(turns int, c distributorChannels, broker *Broker) {
	paused := false
	turn := 0

	for turn < turns {
		select {
		case cmd := <-c.keyPresses:
			handleKeyPress(cmd, &paused, turn, c, world)
			if cmd == 'q' {
				return
			}
		default:
			if !paused {
				evolveToNextGeneration(turn, c, broker, world)
				turn++
			}
		}
	}
}

func aliveCellsInRow(row []Cell, y int) []util.Cell {
	var alive []util.Cell
	for x, cell := range row {
		if cell.Alive {
			alive = append(alive, util.Cell{X: x, Y: y})
		}
	}
	return alive
}

func (world *World) alive() []util.Cell {
	var alive []util.Cell
	for y, row := range world.Field.Data {
		alive = append(alive, aliveCellsInRow(row, y)...)
	}
	return alive
}

func generateFilename(world *World, turn int) string {
	return fmt.Sprintf("%vx%vx%v", world.Width, world.Width, turn)
}

func saveWorldToFile(world *World, c distributorChannels) {
	for y := 0; y < world.Height; y++ {
		for x := 0; x < world.Width; x++ {
			var aliveValue uint8
			if world.Field.Data[y][x].Alive {
				aliveValue = 255
			}
			c.ioOutput <- aliveValue
		}
	}
}

func (world *World) save(turn int, c distributorChannels) {
	filename := generateFilename(world, turn)
	c.ioCommand <- ioOutput
	c.ioFilename <- filename
	saveWorldToFile(world, c)
	c.events <- ImageOutputComplete{
		CompletedTurns: turn,
		Filename:       filename,
	}
}

type BrokerState struct {
	turns int
	cells int
}

type Broker struct {
	State    BrokerState
	GetTurns chan int
	IncTurn  chan struct{}
	GetCells chan int
	SetCells chan int
	Stop     chan bool
}

func (broker *Broker) start() {
	for {
		select {
		case broker.GetTurns <- broker.State.turns:
		case <-broker.IncTurn:
			broker.State.turns++
		case broker.GetCells <- broker.State.cells:
		case broker.State.cells = <-broker.SetCells:
		case <-broker.Stop:
			return
		}
	}
}

type Reporter struct {
	EventsCh       chan<- Event
	Broker         *Broker
	ReportInterval time.Duration
	Stop           chan bool
}

func (reporter *Reporter) start() {
	initialDelay := time.After(InitialDelay)
	ticker := time.NewTicker(reporter.ReportInterval)
	defer ticker.Stop()

	for {
		select {
		case <-initialDelay:
			// Initial delay elapsed, start reporting
		case <-ticker.C:
			turns := <-reporter.Broker.GetTurns
			cellsCount := <-reporter.Broker.GetCells
			log.Printf("Turns: %d, Alive Cells: %d\n", turns, cellsCount)
			reporter.EventsCh <- AliveCellsCount{
				CompletedTurns: turns,
				CellsCount:     cellsCount,
			}
		case <-reporter.Stop:
			// Stop signal received, exit the loop
			return
		}
	}
}

func distributor(p Params, c distributorChannels) {

	filename := fmt.Sprintf("%vx%v", p.ImageWidth, p.ImageHeight)

	c.ioCommand <- ioInput

	c.ioFilename <- filename

	field := Field{
		Height: p.ImageHeight,
		Width:  p.ImageWidth,
	}
	field.cultivate(p.ImageHeight, p.ImageWidth)

	world := World{
		Field:   field,
		Height:  p.ImageHeight,
		Width:   p.ImageWidth,
		Threads: p.Threads,
	}
	world.populate(c)

	broker := Broker{
		GetTurns: make(chan int),
		IncTurn:  make(chan struct{}),
		GetCells: make(chan int),
		SetCells: make(chan int),
		Stop:     make(chan bool),
	}

	go broker.start()

	reporter := Reporter{
		EventsCh:       c.events,
		Broker:         &broker,
		ReportInterval: InitialDelay,
		Stop:           make(chan bool),
	}

	go reporter.start()

	world.Life(p.Turns, c, &broker)

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
