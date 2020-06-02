package scrapers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/adshao/go-binance"
	"github.com/diadata-org/diadata/pkg/dia"
	"github.com/diadata-org/diadata/pkg/dia/helpers"
	utils "github.com/diadata-org/diadata/pkg/utils"
	log "github.com/sirupsen/logrus"
)

// BinanceScraper is a Scraper for collecting trades from the Binance websocket API
type BinanceScraper struct {
	client *binance.Client
	// signaling channels for session initialization and finishing
	initDone     chan nothing
	shutdown     chan nothing
	shutdownDone chan nothing
	// error handling; to read error or closed, first acquire read lock
	// only cleanup method should hold write lock
	errorLock sync.RWMutex
	error     error
	closed    bool
	// used to keep track of trading pairs that we subscribed to
	// use sync.Maps to concurrently handle multiple pairs
	pairScrapers      sync.Map // dia.Pair -> binancePairScraperSet
	pairSubscriptions sync.Map // dia.Pair -> string (subscription ID)
	pairLocks         sync.Map // dia.Pair -> sync.Mutex
	exchangeName      string
	chanTrades        chan *dia.Trade
}

// BinancePairScraper implements PairScraper for Binance
type BinancePairScraper struct {
	parent *BinanceScraper
	pair   dia.Pair
	closed bool
}

type binancePairScraperSet map[*BinancePairScraper]nothing

// NewBinanceScraper returns a new BinanceScraper for the given pair
func NewBinanceScraper(apiKey string, secretKey string, exchangeName string) *BinanceScraper {

	s := &BinanceScraper{
		client:       binance.NewClient(apiKey, secretKey),
		initDone:     make(chan nothing),
		shutdown:     make(chan nothing),
		shutdownDone: make(chan nothing),
		exchangeName: exchangeName,
		error:        nil,
		chanTrades:   make(chan *dia.Trade),
	}

	// establish connection in the background

	go s.mainLoop()
	return s
}

// ScrapePair returns a PairScraper that can be used to get trades for a single pair from
// this APIScraper
func (s *BinanceScraper) ScrapePair(pair dia.Pair) (PairScraper, error) {
	<-s.initDone // wait until client is connected

	if s.closed {
		return nil, errors.New("BinanceScraper: Call ScrapePair on closed scraper")
	}

	ps := &BinancePairScraper{
		parent: s,
		pair:   pair,
	}

	wsAggTradeHandler := func(event *binance.WsAggTradeEvent) {

		volume, err := strconv.ParseFloat(event.Quantity, 64)
		price, err2 := strconv.ParseFloat(event.Price, 64)

		if err == nil && err2 == nil && event.Event == "aggTrade" {
			if event.IsBuyerMaker == false {
				volume = -volume
			}
			t := &dia.Trade{
				Symbol:         pair.Symbol,
				Pair:           pair.ForeignName,
				Price:          price,
				Volume:         volume,
				Time:           time.Unix(event.TradeTime/1000, (event.TradeTime%1000)*int64(time.Millisecond)),
				ForeignTradeID: strconv.FormatInt(event.AggTradeID, 16),
				Source:         s.exchangeName,
			}
			ps.parent.chanTrades <- t
		} else {
			log.Println("ignoring event ", event, err, err2)
		}
	}
	errHandler := func(err error) {
		fmt.Println(err)
	}

	_, _, err := binance.WsAggTradeServe(pair.ForeignName, wsAggTradeHandler, errHandler)

	return ps, err
}

// runs in a goroutine until s is closed
func (s *BinanceScraper) mainLoop() {
	close(s.initDone)
	for {
		select {
		case <-s.shutdown: // user requested shutdown
			log.Println("BinanceScraper shutting down")
			s.cleanup(nil)
			return
		}
	}
}

// FetchAvailablePairs returns a list with all available trade pairs
func (s *BinanceScraper) FetchAvailablePairs() (pairs []dia.Pair, err error) {

	data, err := utils.GetRequest("https://api.binance.com//api/v1/exchangeInfo")

	if err != nil {
		return
	}
	var ar binance.ExchangeInfo
	err = json.Unmarshal(data, &ar)
	if err == nil {
		for _, p := range ar.Symbols {
			symbol, serr := s.normalizeSymbol(p.Symbol, p.BaseAsset, p.Status)
			if serr == nil {
				pairs = append(pairs, dia.Pair{
					Symbol:      symbol,
					ForeignName: p.Symbol,
					Exchange:    s.exchangeName,
				})
			} else {
				log.Error(serr)
			}
		}
	}
	return
}

func eventHandler(event *binance.WsAggTradeEvent) {
	fmt.Println(event)
}

// closes all connected PairScrapers
// must only be called from mainLoop
func (s *BinanceScraper) cleanup(err error) {
	s.errorLock.Lock()
	defer s.errorLock.Unlock()
	// close all channels of PairScraper children
	s.pairScrapers.Range(func(k, v interface{}) bool {
		for ps := range v.(binancePairScraperSet) {
			ps.closed = true
		}
		s.pairScrapers.Delete(k)
		return true
	})

	s.closed = true
	close(s.shutdownDone) // signal that shutdown is complete
}

// Close closes any existing API connections, as well as channels of
// PairScrapers from calls to ScrapePair
func (s *BinanceScraper) Close() error {
	if s.closed {
		return errors.New("BinanceScraper: Already closed")
	}
	close(s.shutdown)
	<-s.shutdownDone
	s.errorLock.RLock()
	defer s.errorLock.RUnlock()
	return s.error
}

// Close stops listening for trades of the pair associated with s
func (ps *BinancePairScraper) Close() error {
	var err error
	s := ps.parent
	// if parent already errored, return early
	s.errorLock.RLock()
	defer s.errorLock.RUnlock()
	if s.error != nil {
		return s.error
	}
	if ps.closed {
		return errors.New("BinancePairScraper: Already closed")
	}

	// TODO stop collection for the pair

	ps.closed = true
	return err
}

func (s *BinanceScraper) normalizeSymbol(foreignName string, params ...string) (symbol string, err error) {
	symbol = params[0]
	status := params[1]
	if status == "TRADING" {
		if helpers.NameForSymbol(symbol) == symbol {
			if !helpers.SymbolIsName(symbol) {
				if symbol == "IOTA" {
					return "MIOTA", nil
				}
				if symbol == "YOYO" {
					return "YOYOW", nil
				}
				/// ethos
				if symbol == "BQX" {
					return "ETHOS", nil
				}
				/// Bitcoin Cash
				if symbol == "BCC" {
					return "BCH", nil
				}
				return symbol, errors.New("Foreign name can not be normalized:" + foreignName + " symbol:" + symbol)
			}
		}
		if helpers.SymbolIsBlackListed(symbol) {
			return symbol, errors.New("Symbol is black listed:" + symbol)
		}
	} else {
		return symbol, errors.New("Symbol:" + symbol + " with foreign name:" + foreignName + " is:" + status)

	}
	return symbol, nil
}

// Channel returns a channel that can be used to receive trades
func (ps *BinanceScraper) Channel() chan *dia.Trade {
	return ps.chanTrades
}

// Error returns an error when the channel Channel() is closed
// and nil otherwise
func (ps *BinancePairScraper) Error() error {
	s := ps.parent
	s.errorLock.RLock()
	defer s.errorLock.RUnlock()
	return s.error
}

func errorHandler(err error) {
	fmt.Println(err)
}

// Pair returns the pair this scraper is subscribed to
func (ps *BinancePairScraper) Pair() dia.Pair {
	return ps.pair
}
