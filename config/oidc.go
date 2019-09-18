package config

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gopkg.in/square/go-jose.v2/jwt"
)

func init() {
	monitors = make(map[string]chan bool)
	monitorsLock = &sync.Mutex{}
}

var (
	monitors     map[string]chan bool
	monitorsLock *sync.Mutex
)

type Authority struct {
	Id  string `json:"id"`
	URI string `json:"uri"`

	ServerLabel string    `json:"serverLabel"`
	Username    string    `json:"username"`
	LoginDate   time.Time `json:"loginDate"`
	RefreshDate time.Time `json:"refreshDate"`
	TokenStatus string    `json:"tokenStatus"`

	IdToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int    `json:"expires_at"`
}

type AuthChange struct {
	Type      string
	Authority *Authority
}

func (a *Authority) RefreshRequired() (in time.Duration, now bool) {
	expTime := time.Unix(int64(a.ExpiresAt), 0)
	in = expTime.Sub(time.Now().Add(30 * time.Second))
	if in <= 0 {
		in = 0
		now = true
		fmt.Println("Token expired, should refresh now")
	} else {
		fmt.Println("Will refresh in", in)
	}
	return
}

func (a *Authority) Refresh() error {

	fmt.Println("Refreshing token for ", a.URI)
	data := url.Values{}
	data.Add("grant_type", "refresh_token")
	data.Add("client_id", "cells-sync")
	data.Add("refresh_token", a.RefreshToken)
	data.Add("scope", "openid email profile pydio offline")
	httpReq, err := http.NewRequest("POST", a.URI+"/oidc/oauth2/token", strings.NewReader(data.Encode()))
	if err != nil {
		return err
	}
	httpReq.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Add("Cache-Control", "no-cache")

	client := http.DefaultClient
	res, err := client.Do(httpReq)
	if err != nil {
		return err
	} else if res.StatusCode != 200 {
		bb, _ := ioutil.ReadAll(res.Body)
		return fmt.Errorf("received status code %d - %s", res.StatusCode, string(bb))
	}
	defer res.Body.Close()
	var respMap struct {
		Id      string `json:"id_token"`
		Refresh string `json:"refresh_token"`
		Exp     int    `json:"expires_in"`
	}
	err = json.NewDecoder(res.Body).Decode(&respMap)
	if err != nil {
		return fmt.Errorf("could not unmarshall response with status %d: %s\nerror cause: %s", res.StatusCode, res.Status, err.Error())
	}
	a.IdToken = respMap.Id
	a.RefreshToken = respMap.Refresh
	a.ExpiresAt = int(time.Now().Unix()) + respMap.Exp
	fmt.Println("Got new token, will expire in ", respMap.Exp, " thus expiresAt ", a.ExpiresAt)

	Default().UpdateAuthority(a, true)

	return nil
}

func (a *Authority) LoadInfo() {
	a.ServerLabel = a.URI
	if r, e := http.Get(strings.TrimRight(a.URI, "/") + "/a/frontend/bootconf"); e == nil {
		var confSample struct {
			Wording struct {
				Title      string `json:"title"`
				Icon       string `json:"icon"`
				IconBinary string `json:"iconBinary"`
			} `json:"customWording"`
			Backend struct {
				PackageLabel string `json:"packageLabel"`
			} `json:"backend"`
		}
		bb, _ := ioutil.ReadAll(r.Body)
		if e := json.Unmarshal(bb, &confSample); e == nil {
			if confSample.Wording.Title != "" {
				a.ServerLabel = confSample.Wording.Title
			}
		}
	}
	// decode JWT token without verifying the signature
	token, _ := jwt.ParseSigned(a.IdToken)
	var claims map[string]interface{} // generic map to store parsed token
	_ = token.UnsafeClaimsWithoutVerification(&claims)
	if name, ok := claims["name"]; ok {
		a.Username = name.(string)
	}
	parsed, _ := url.Parse(a.URI)
	parsed.User = url.User(a.Username)
	a.Id = parsed.String()
}

func (a *Authority) key() string {
	if a.Id == "" {
		a.LoadInfo()
	}
	return a.Id
}

func (a *Authority) is(a2 *Authority) bool {
	return a.key() == a2.key()
}

func monitorToken(a *Authority) {

	var done chan bool
	monitorsLock.Lock()
	if d, ok := monitors[a.key()]; ok {
		done = d
	} else {
		done = make(chan bool, 1)
		monitors[a.key()] = done
	}
	monitorsLock.Unlock()
	d, _ := a.RefreshRequired()
	for {
		select {
		case <-time.After(d):
			if e := a.Refresh(); e != nil {
				fmt.Println(e)
				stopMonitoringToken(a.key())
			} else {
				monitorToken(a)
			}
			return
		case <-done:
			fmt.Println("Stopping monitor on " + a.key())
			return
		}
	}
}

func stopMonitoringToken(key string) {
	monitorsLock.Lock()
	if done, ok := monitors[key]; ok {
		close(done)
		delete(monitors, key)
	}
	monitorsLock.Unlock()
}

func (g *Global) PublicAuthorities() []*Authority {
	var p []*Authority
	for _, a := range g.Authorities {
		p = append(p, &Authority{
			Id:          a.key(),
			URI:         a.URI,
			ServerLabel: a.ServerLabel,
			Username:    a.Username,
			RefreshDate: a.RefreshDate,
			LoginDate:   a.LoginDate,
			ExpiresAt:   a.ExpiresAt,
		})
	}
	return p
}

func (g *Global) CreateAuthority(a *Authority) error {
	a.LoadInfo()
	for _, auth := range g.Authorities {
		if auth.is(a) {
			return g.UpdateAuthority(a, false)
		}
	}
	a.LoginDate = time.Now()
	a.LoadInfo()
	g.Authorities = append(g.Authorities, a)
	e := Save()
	if e == nil {
		go func() {
			for _, c := range g.changes {
				c <- &AuthChange{Type: "create", Authority: a}
			}
		}()
		go monitorToken(a)
	}
	return e
}

func (g *Global) RemoveAuthority(a *Authority) error {
	var newAuths []*Authority
	for _, auth := range g.Authorities {
		if !a.is(auth) {
			newAuths = append(newAuths, auth)
		}
	}
	g.Authorities = newAuths
	e := Save()
	if e == nil {
		go func() {
			for _, c := range g.changes {
				c <- &AuthChange{Type: "remove", Authority: a}
			}
		}()
		stopMonitoringToken(a.key())
	}
	return e
}

func (g *Global) UpdateAuthority(a *Authority, isRefresh bool) error {
	if !isRefresh {
		a.LoginDate = time.Now()
	} else {
		a.RefreshDate = time.Now()
	}
	for _, auth := range g.Authorities {
		if auth.is(a) {
			auth.IdToken = a.IdToken
			auth.RefreshToken = a.RefreshToken
			if isRefresh {
				auth.RefreshDate = time.Now()
			} else {
				auth.LoginDate = time.Now()
			}
		}
	}
	e := Save()
	if e == nil {
		go func() {
			for _, c := range g.changes {
				c <- &AuthChange{Type: "update", Authority: a}
			}
		}()
	}
	return e
}
