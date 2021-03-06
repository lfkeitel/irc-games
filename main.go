package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	irc "github.com/lfkeitel/goirc/client"
	"github.com/lfkeitel/goirc/logging"
)

var (
	ircServer      string
	ircNick        string
	ircPort        int
	ircUseTLS      bool
	ircInsecureTLS bool
	ircChans       string
	debug          bool
	debug2         bool

	useSASL      bool
	saslLogin    string
	saslPassword string
)

func init() {
	flag.StringVar(&ircServer, "s", "127.0.0.1", "IRC server")
	flag.StringVar(&ircNick, "n", "gamerbot", "IRC nick")
	flag.IntVar(&ircPort, "p", 6667, "IRC port")
	flag.BoolVar(&ircUseTLS, "tls", false, "Use TLS")
	flag.BoolVar(&ircInsecureTLS, "insecure", false, "Ignore TLS cert errors")
	flag.StringVar(&ircChans, "c", "#games", "Comma separated list of channels to join")
	flag.BoolVar(&debug, "debug", false, "Enable debug output")
	flag.BoolVar(&debug, "debug2", false, "Enable extra debug output")

	flag.BoolVar(&useSASL, "sasl", false, "Use SASL authentication, forces TLS")
	flag.StringVar(&saslLogin, "sasluser", "", "SASL username if different from nick")
	flag.StringVar(&saslPassword, "saslpass", "", "SASL password")

	rand.Seed(time.Now().UnixNano())
}

func main() {
	flag.Parse()

	if debug2 {
		logging.SetLogger(&logging.StdoutLogger{})
		debug = true
	}

	if useSASL && saslLogin == "" {
		saslLogin = ircNick
	}

	if useSASL {
		ircUseTLS = true
	}

	cfg := irc.NewConfig(ircNick)
	cfg.SSL = ircUseTLS
	cfg.SSLConfig = &tls.Config{InsecureSkipVerify: ircInsecureTLS}
	cfg.Server = fmt.Sprintf("%s:%d", ircServer, ircPort)
	cfg.NewNick = func(n string) string { return n + "^" }
	cfg.Me.Ident = ircNick

	cfg.UseSASL = useSASL
	cfg.SASLLogin = saslLogin
	cfg.SASLPassword = saslPassword
	c := irc.Client(cfg)

	chans := strings.Split(ircChans, ",")

	c.HandleFunc(irc.CONNECTED, func(conn *irc.Conn, line *irc.Line) {
		fmt.Println("Connected to IRC server, joining channels")
		for _, channel := range chans {
			if channel[0] == '#' {
				fmt.Printf("Joining %s\n", channel)
				conn.Join(channel)
			}
		}
	})

	quit := make(chan bool)
	c.HandleFunc(irc.DISCONNECTED, func(conn *irc.Conn, line *irc.Line) { close(quit) })

	c.HandleFunc(irc.ERROR, func(conn *irc.Conn, line *irc.Line) {
		if !strings.HasPrefix(line.Args[0], "Closing Link") {
			fmt.Println(line.Raw)
		}
	})

	c.HandleFunc(irc.PRIVMSG, func(conn *irc.Conn, line *irc.Line) {
		if debug {
			fmt.Printf("%#v\n", line)
		}

		defer func() {
			if r := recover(); r != nil {
				fmt.Println(r)
			}
		}()
		processMsg(conn, line)
	})

	fmt.Println("Connecting to IRC server")
	if err := c.Connect(); err != nil {
		fmt.Printf("Connection error: %s\n", err.Error())
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	select {
	case <-quit:
		fmt.Println("Client disconnected from server")
		return
	case <-shutdown:
		fmt.Println("\nDisconnecting from server")
		for _, channel := range chans {
			if channel[0] == '#' {
				fmt.Printf("Leaving %s\n", channel)
				c.Part(channel, "Bye, bye")
			}
		}
		c.Quit("Bye everyone!")
		<-time.After(1 * time.Second) // Give messages time to send
		c.Close()
	}

	select {
	case <-quit:
		fmt.Println("Disconnected")
	case <-time.After(5 * time.Second):
		fmt.Println("Server took too long disconnecting")
	}
}

func processMsg(conn *irc.Conn, line *irc.Line) {
	if len(line.Args) < 2 {
		conn.Notice(line.Nick, "Try '.help' instead.")
		return
	}

	args := parseCommandLine(line.Args[1])

	// If in message received from a channel, only response to "dot" commands
	if line.Args[0][0] == '#' && args[0][0] != '.' {
		return
	}

	cmd := strings.ToLower(args[0])
	if len(cmd) == 0 {
		cmd = ".help"
	} else if cmd[0] != '.' {
		cmd = "." + cmd
	}

	args = args[1:]
	recipient := line.Args[0]
	if recipient == ircNick {
		recipient = line.Nick
	}

	if debug {
		fmt.Printf("%s %#v\n", cmd, args)
	}

	if strings.HasPrefix(cmd, ".hello") {
		conn.Privmsgf(recipient, "Hi %s! Want to play a game?", line.Nick)
		return
	}

	switch cmd {
	case ".help":
		conn.Privmsg(recipient, "If you want to play a game, say '.play <game>'.")
	case ".yea", ".yes":
		conn.Privmsg(recipient, "What game do you want to play? '.play <game>'.")
	case ".play":
		startGame(conn, line, args)
	case ".stop":
		stopGame(conn, line, args)
	case ".games":
		conn.Privmsg(recipient, "Available games: guess.")
	case ".playing":
		if hasActiveGame(line.Nick) {
			conn.Privmsgf(recipient, "You're playing %s.", getGame(line.Nick).id())
		} else {
			conn.Privmsg(recipient, "You're not playing a game. Start one by saying '.play <game>'.")
		}
	default:
		if hasActiveGame(line.Nick) {
			getGame(line.Nick).play(conn, line, append([]string{cmd}, args...))
		} else {
			conn.Notice(recipient, "Try '.help' instead.")
		}
	}
}

func parseCommandLine(line string) []string {
	return strings.Split(line, " ")
}

func startGame(conn *irc.Conn, line *irc.Line, args []string) {
	if hasActiveGame(line.Nick) {
		conn.Notice(line.Nick, "You're already playing a game. Please stop your current game first.")
		return
	}

	if len(args) != 1 {
		conn.Notice(line.Nick, "I need to know what game you want to play.")
		conn.Notice(line.Nick, "Use the 'games' command to see what I have.")
		return
	}

	switch args[0] {
	case guessingGameID:
		setGame(line.Nick, newGuessingGame())
		getGame(line.Nick).start(conn, line)
	default:
		conn.Notice(line.Nick, "Use the 'games' command to see what I have.")
	}
	fmt.Printf("User %s started game %s\n", line.Nick, args[0])
}

func stopGame(conn *irc.Conn, line *irc.Line, args []string) {
	if !hasActiveGame(line.Nick) {
		conn.Notice(line.Nick, "You're not playing a game right now.")
		return
	}

	if len(args) == 0 {
		conn.Notice(line.Nick, "Are you sure you want to stop the game? Say '.stop y'.")
		return
	}

	response := strings.ToLower(args[0])
	if response == "y" || response == "yes" {
		getGame(line.Nick).stop(conn, line)
		conn.Notice(line.Nick, "I was just beginning to have fun...")
	}
}
