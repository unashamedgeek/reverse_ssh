package users

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

	"github.com/NHAS/reverse_ssh/internal"
	"github.com/NHAS/reverse_ssh/pkg/trie"
	"golang.org/x/crypto/ssh"
)

const (
	UserPermissions  = 0
	AdminPermissions = 5
)

var ErrNilServerConnection = errors.New("the server connection was nil for the client")

var (
	lck sync.RWMutex
	// Username to actual user object
	users = map[string]*User{}

	activeConnections = map[string]bool{}
)

type Connection struct {
	// This is the users connection to the server itself, creates new channels and whatnot. NOT to get io.Copy'd
	serverConnection ssh.Conn

	Pty *internal.PtyReq

	ShellRequests <-chan *ssh.Request

	// So we can capture details about who is currently using the rssh server
	ConnectionDetails string
}

type User struct {
	sync.RWMutex

	userConnections map[string]*Connection
	username        string

	clients      map[string]*ssh.ServerConn
	autocomplete *trie.Trie

	privilege *int
}

func (u *User) SearchClients(filter string) (out map[string]*ssh.ServerConn, err error) {

	filter = filter + "*"
	_, err = filepath.Match(filter, "")
	if err != nil {
		return nil, fmt.Errorf("filter is not well formed")
	}

	out = make(map[string]*ssh.ServerConn)

	lck.RLock()
	defer lck.RUnlock()

	searchClients := u.clients

	if u.Privilege() == AdminPermissions {
		searchClients = allClients
	}

	for id, conn := range searchClients {
		if filter == "" {
			out[id] = conn
			continue
		}

		if _matches(filter, id, conn.RemoteAddr().String()) {
			out[id] = conn
			continue
		}

	}

	return
}

func _matches(filter, clientId, remoteAddr string) bool {
	match, _ := filepath.Match(filter, clientId)
	if match {
		return true
	}

	for _, alias := range uniqueIdToAllAliases[clientId] {
		match, _ = filepath.Match(filter, alias)
		if match {
			return true
		}
	}

	match, _ = filepath.Match(filter, remoteAddr)
	return match
}

// Matches tests if any of the client IDs match
func (u *User) Matches(filter, clientId, remoteAddr string) bool {
	lck.RLock()
	defer lck.RUnlock()

	return _matches(filter, clientId, remoteAddr)
}

func (u *User) GetClient(identifier string) (ssh.Conn, error) {
	lck.RLock()
	defer lck.RUnlock()

	if m, ok := allClients[identifier]; ok {
		return m, nil
	}

	searchClients := u.clients
	if u.Privilege() == AdminPermissions {
		searchClients = allClients
	}

	matchingUniqueIDs, ok := aliases[identifier]
	if !ok {
		return nil, fmt.Errorf("%s not found", identifier)
	}

	if u.Privilege() != AdminPermissions {

		// If the user is not an admin, check to make sure that the id's are part of the users ownership, if they arent, delete them
		toRemove := []string{}
		for id := range matchingUniqueIDs {
			_, ok := u.clients[id]
			if !ok {
				toRemove = append(toRemove, id)
			}
		}

		for _, id := range toRemove {
			delete(matchingUniqueIDs, id)
		}
	}

	if len(matchingUniqueIDs) == 1 {
		// Easiest way to get a single item from a map
		for k := range matchingUniqueIDs {
			return searchClients[k], nil
		}
	}

	matches := 0
	matchingHosts := ""
	for k := range matchingUniqueIDs {
		matches++
		client := searchClients[k]
		matchingHosts += fmt.Sprintf("%s (%s %s)\n", k, client.User(), client.RemoteAddr().String())
	}

	if len(matchingHosts) > 0 {
		matchingHosts = matchingHosts[:len(matchingHosts)-1]
	}
	return nil, fmt.Errorf("%d connections match alias '%s'\n%s", matches, identifier, matchingHosts)

}

func (u *User) Autocomplete() *trie.Trie {
	if u.privilege != nil && *u.privilege == AdminPermissions {
		return globalAutoComplete
	}

	return u.autocomplete
}

func (u *User) Session(connectionDetails string) (*Connection, error) {
	if c, ok := u.userConnections[connectionDetails]; ok {
		return c, nil
	}

	return nil, errors.New("session not found")
}

func (u *User) Username() string {
	return u.username
}

func (u *User) Privilege() int {

	if u.privilege == nil {
		log.Println("was unable to get privs of", u.username, "defaulting to 0 (no priv)")

		return 0
	}

	return *u.privilege
}

func GetUser(username string) (*User, error) {

	lck.RLock()
	defer lck.RUnlock()

	return _getUser(username)
}

// Non-threadsafe variant, used internally when outer function is locked
func _getUser(username string) (*User, error) {
	u, ok := users[username]
	if !ok {
		return nil, errors.New("not found")
	}

	return u, nil
}

func CreateOrGetUser(username string, serverConnection *ssh.ServerConn) (us *User, connectionDetails string, err error) {
	lck.Lock()
	defer lck.Unlock()

	return _createOrGetUser(username, serverConnection)
}

func _createOrGetUser(username string, serverConnection *ssh.ServerConn) (us *User, connectionDetails string, err error) {
	u, ok := users[username]
	if !ok {
		newUser := &User{
			username:        username,
			userConnections: map[string]*Connection{},
			autocomplete:    trie.NewTrie(),
			clients:         make(map[string]*ssh.ServerConn),
		}

		users[username] = newUser
		u = newUser
	}

	if serverConnection != nil {
		newConnection := &Connection{
			serverConnection:  serverConnection,
			ShellRequests:     make(<-chan *ssh.Request),
			ConnectionDetails: makeConnectionDetailsString(serverConnection),
		}

		priv, err := strconv.Atoi(serverConnection.Permissions.Extensions["privilege"])
		if err != nil {
			log.Println("could not parse privileges: ", err)
		} else {
			u.privilege = &priv
		}

		if _, ok := u.userConnections[newConnection.ConnectionDetails]; ok {
			return nil, "", fmt.Errorf("connection already exists for %s", newConnection.ConnectionDetails)
		}

		u.userConnections[newConnection.ConnectionDetails] = newConnection
		activeConnections[newConnection.ConnectionDetails] = true

		return u, newConnection.ConnectionDetails, nil
	}

	return u, "", nil
}

func makeConnectionDetailsString(ServerConnection *ssh.ServerConn) string {
	return fmt.Sprintf("%s@%s", ServerConnection.User(), ServerConnection.RemoteAddr().String())
}

func ListUsers() (userList []string) {
	lck.RLock()
	defer lck.RUnlock()

	for user := range users {
		userList = append(userList, user)
	}

	sort.Strings(userList)
	return
}

func DisconnectUser(ServerConnection *ssh.ServerConn) {
	if ServerConnection != nil {
		lck.Lock()
		defer lck.Unlock()
		defer ServerConnection.Close()

		details := makeConnectionDetailsString(ServerConnection)

		user, ok := users[ServerConnection.User()]
		if !ok {
			return
		}

		delete(user.userConnections, details)
		delete(activeConnections, details)

		if len(user.clients) == 0 {
			delete(users, user.username)
		}
	}
}
