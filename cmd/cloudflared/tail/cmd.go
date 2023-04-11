package tail

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mattn/go-colorable"
	"github.com/rs/zerolog"
	"github.com/urfave/cli/v2"
	"nhooyr.io/websocket"

	"github.com/cloudflare/cloudflared/logger"
	"github.com/cloudflare/cloudflared/management"
)

var (
	version string
)

func Init(v string) {
	version = v
}

func Command() *cli.Command {
	return &cli.Command{
		Name:   "tail",
		Action: Run,
		Usage:  "Stream logs from a remote cloudflared",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "connector-id",
				Usage:   "Access a specific cloudflared instance by connector id (for when a tunnel has multiple cloudflared's)",
				Value:   "",
				EnvVars: []string{"TUNNEL_MANAGEMENT_CONNECTOR"},
			},
			&cli.StringSliceFlag{
				Name:    "event",
				Usage:   "Filter by specific Events (cloudflared, http, tcp, udp) otherwise, defaults to send all events",
				EnvVars: []string{"TUNNEL_MANAGEMENT_FILTER_EVENTS"},
			},
			&cli.StringFlag{
				Name:    "level",
				Usage:   "Filter by specific log levels (debug, info, warn, error)",
				EnvVars: []string{"TUNNEL_MANAGEMENT_FILTER_LEVEL"},
				Value:   "debug",
			},
			&cli.StringFlag{
				Name:    "token",
				Usage:   "Access token for a specific tunnel",
				Value:   "",
				EnvVars: []string{"TUNNEL_MANAGEMENT_TOKEN"},
			},
			&cli.StringFlag{
				Name:    "management-hostname",
				Usage:   "Management hostname to signify incoming management requests",
				EnvVars: []string{"TUNNEL_MANAGEMENT_HOSTNAME"},
				Hidden:  true,
				Value:   "management.argotunnel.com",
			},
			&cli.StringFlag{
				Name:   "trace",
				Usage:  "Set a cf-trace-id for the request",
				Hidden: true,
				Value:  "",
			},
			&cli.StringFlag{
				Name:    logger.LogLevelFlag,
				Value:   "info",
				Usage:   "Application logging level {debug, info, warn, error, fatal}",
				EnvVars: []string{"TUNNEL_LOGLEVEL"},
			},
		},
	}
}

// Middleware validation error struct for returning to the eyeball
type managementError struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Middleware validation error HTTP response JSON for returning to the eyeball
type managementErrorResponse struct {
	Success bool              `json:"success,omitempty"`
	Errors  []managementError `json:"errors,omitempty"`
}

func handleValidationError(resp *http.Response, log *zerolog.Logger) {
	if resp.StatusCode == 530 {
		log.Error().Msgf("no cloudflared connector available or reachable via management request (a recent version of cloudflared is required to use streaming logs)")
	}
	var managementErr managementErrorResponse
	err := json.NewDecoder(resp.Body).Decode(&managementErr)
	if err != nil {
		log.Error().Msgf("unable to start management log streaming session: http response code returned %d", resp.StatusCode)
		return
	}
	if managementErr.Success || len(managementErr.Errors) == 0 {
		log.Error().Msgf("management tunnel validation returned success with invalid HTTP response code to convert to a WebSocket request")
		return
	}
	for _, e := range managementErr.Errors {
		log.Error().Msgf("management request failed validation: (%d) %s", e.Code, e.Message)
	}
}

// logger will be created to emit only against the os.Stderr as to not obstruct with normal output from
// management requests
func createLogger(c *cli.Context) *zerolog.Logger {
	level, levelErr := zerolog.ParseLevel(c.String(logger.LogLevelFlag))
	if levelErr != nil {
		level = zerolog.InfoLevel
	}
	log := zerolog.New(zerolog.ConsoleWriter{
		Out:        colorable.NewColorable(os.Stderr),
		TimeFormat: time.RFC3339,
	}).With().Timestamp().Logger().Level(level)
	return &log
}

// parseFilters will attempt to parse provided filters to send to with the EventStartStreaming
func parseFilters(c *cli.Context) (*management.StreamingFilters, error) {
	var level *management.LogLevel
	var events []management.LogEventType

	argLevel := c.String("level")
	argEvents := c.StringSlice("event")

	if argLevel != "" {
		l, ok := management.ParseLogLevel(argLevel)
		if !ok {
			return nil, fmt.Errorf("invalid --level filter provided, please use one of the following Log Levels: debug, info, warn, error")
		}
		level = &l
	}

	for _, v := range argEvents {
		t, ok := management.ParseLogEventType(v)
		if !ok {
			return nil, fmt.Errorf("invalid --event filter provided, please use one of the following EventTypes: cloudflared, http, tcp, udp")
		}
		events = append(events, t)
	}

	if level == nil && len(events) == 0 {
		// When no filters are provided, do not return a StreamingFilters struct
		return nil, nil
	}

	return &management.StreamingFilters{
		Level:  level,
		Events: events,
	}, nil
}

// Run implements a foreground runner
func Run(c *cli.Context) error {
	log := createLogger(c)

	signals := make(chan os.Signal, 10)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)

	filters, err := parseFilters(c)
	if err != nil {
		log.Error().Err(err).Msgf("invalid filters provided")
		return nil
	}

	managementHostname := c.String("management-hostname")
	token := c.String("token")
	u := url.URL{Scheme: "wss", Host: managementHostname, Path: "/logs", RawQuery: "access_token=" + token}

	header := make(http.Header)
	header.Add("User-Agent", "cloudflared/"+version)
	trace := c.String("trace")
	if trace != "" {
		header["cf-trace-id"] = []string{trace}
	}
	ctx := c.Context
	conn, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPHeader: header,
	})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			handleValidationError(resp, log)
			return nil
		}
		log.Error().Err(err).Msgf("unable to start management log streaming session")
		return nil
	}
	defer conn.Close(websocket.StatusInternalError, "management connection was closed abruptly")

	// Once connection is established, send start_streaming event to begin receiving logs
	err = management.WriteEvent(conn, ctx, &management.EventStartStreaming{
		ClientEvent: management.ClientEvent{Type: management.StartStreaming},
		Filters:     filters,
	})
	if err != nil {
		log.Error().Err(err).Msg("unable to request logs from management tunnel")
		return nil
	}

	readerDone := make(chan struct{})

	go func() {
		defer close(readerDone)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				event, err := management.ReadServerEvent(conn, ctx)
				if err != nil {
					if closeErr := management.AsClosed(err); closeErr != nil {
						// If the client (or the server) already closed the connection, don't continue to
						// attempt to read from the client.
						if closeErr.Code == websocket.StatusNormalClosure {
							return
						}
						// Only log abnormal closures
						log.Error().Msgf("received remote closure: (%d) %s", closeErr.Code, closeErr.Reason)
						return
					}
					log.Err(err).Msg("unable to read event from server")
					return
				}
				switch event.Type {
				case management.Logs:
					logs, ok := management.IntoServerEvent(event, management.Logs)
					if !ok {
						log.Error().Msgf("invalid logs event")
						continue
					}
					// Output all the logs received to stdout
					for _, l := range logs.Logs {
						fields, err := json.Marshal(l.Fields)
						if err != nil {
							fields = []byte("unable to parse fields")
							log.Debug().Msgf("unable to parse fields from event %+v", l)
						}
						fmt.Printf("%s %s %s %s %s\n", l.Time, l.Level, l.Event, l.Message, fields)
					}
				case management.UnknownServerEventType:
					fallthrough
				default:
					log.Debug().Msgf("unexpected log event type: %s", event.Type)
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-readerDone:
			return nil
		case <-signals:
			log.Debug().Msg("closing management connection")
			// Cleanly close the connection by sending a close message and then
			// waiting (with timeout) for the server to close the connection.
			conn.Close(websocket.StatusNormalClosure, "")
			select {
			case <-readerDone:
			case <-time.After(time.Second):
			}
			return nil
		}
	}
}