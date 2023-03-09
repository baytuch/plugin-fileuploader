package server

import (
	"errors"
	"fmt"
	"io/ioutil"
	goLog "log"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/dgrijalva/jwt-go"
	"github.com/gin-gonic/gin"
	"github.com/kiwiirc/plugin-fileuploader/db"
	"github.com/kiwiirc/plugin-fileuploader/events"
	"github.com/kiwiirc/plugin-fileuploader/logging"
	"github.com/kiwiirc/plugin-fileuploader/shardedfilestore"
	"github.com/tus/tusd"
	"github.com/tus/tusd/cmd/tusd/cli/hooks"
)

func routePrefixFromBasePath(basePath string) (string, error) {
	url, err := url.Parse(basePath)
	if err != nil {
		return "", err
	}

	return url.Path, nil
}

func customizedCors(allowedOrigins []string) gin.HandlerFunc {
	// convert slice values to keys of map for "contains" test
	originSet := make(map[string]struct{}, len(allowedOrigins))
	exists := struct{}{}
	for _, origin := range allowedOrigins {
		originSet[origin] = exists
	}

	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		respHeader := c.Writer.Header()

		// only allow the origin if it's in the list from the config, * is not supported!
		if _, ok := originSet[origin]; ok {
			respHeader.Set("Access-Control-Allow-Origin", origin)
		} else {
			respHeader.Del("Access-Control-Allow-Origin")
		}

		// lets the user-agent know the response can vary depending on the origin of the request.
		// ensures correct behavior of browser cache.
		respHeader.Add("Vary", "Origin")
	}
}

func (serv *UploadServer) registerTusHandlers(r *gin.Engine, store *shardedfilestore.ShardedFileStore) error {
	composer := tusd.NewStoreComposer()
	store.UseIn(composer)

	maximumUploadSize := serv.cfg.Storage.MaximumUploadSize
	serv.log.Debug().Str("size", maximumUploadSize.String()).Msg("Using upload limit")

	config := tusd.Config{
		BasePath:                serv.cfg.Server.BasePath,
		StoreComposer:           composer,
		MaxSize:                 int64(maximumUploadSize.Bytes()),
		Logger:                  goLog.New(ioutil.Discard, "", 0),
		NotifyCompleteUploads:   true,
		NotifyCreatedUploads:    true,
		NotifyTerminatedUploads: true,
		NotifyUploadProgress:    true,
		RespectForwardedHeaders: true,
	}

	routePrefix, err := routePrefixFromBasePath(serv.cfg.Server.BasePath)
	if err != nil {
		return err
	}

	handler, err := tusd.NewUnroutedHandler(config)
	if err != nil {
		return err
	}

	// create event broadcaster
	serv.tusEventBroadcaster = events.NewTusEventBroadcaster(handler)

	// attach logger
	go logging.TusdLogger(serv.log, serv.tusEventBroadcaster)

	// attach uploader IP recorder
	go serv.ipRecorder(serv.tusEventBroadcaster)

	noopHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	// For unknown reasons, this middleware must be mounted on the top level router.
	// When attached to the RouterGroup, it does not get called for some requests.
	tusdMiddleware := gin.WrapH(handler.Middleware(noopHandler))
	r.Use(tusdMiddleware)
	r.Use(customizedCors(serv.cfg.Server.CorsOrigins))

	rg := r.Group(routePrefix)
	rg.POST("", serv.postFile(handler))
	rg.HEAD(":id", gin.WrapF(handler.HeadFile))
	rg.PATCH(":id", gin.WrapF(handler.PatchFile))

	// Only attach the DELETE handler if the Terminate() method is provided
	if config.StoreComposer.UsesTerminater {
		rg.DELETE(":id", gin.WrapF(handler.DelFile))
	}

	// GET handler requires the GetReader() method
	if config.StoreComposer.UsesGetReader {
		getFile := gin.WrapF(handler.GetFile)
		rg.GET(":id", getFile)
		rg.GET(":id/:filename", func(c *gin.Context) {
			// rewrite request path to ":id" route pattern
			c.Request.URL.Path = path.Join(routePrefix, url.PathEscape(c.Param("id")))

			// call the normal handler
			getFile(c)
		})
	}

	return nil
}

func isFatalJwtError(err error) (fatal bool) {
	fatal = true

	// jwt.ValidationError<UnknownIssuerError> => non-fatal
	if jwtValidationErr, ok := err.(*jwt.ValidationError); ok {
		if _, ok := jwtValidationErr.Inner.(*UnknownIssuerError); ok {
			fatal = false
			return
		}
	}

	return
}

func (serv *UploadServer) postFile(handler *tusd.UnroutedHandler) gin.HandlerFunc {
	return func(c *gin.Context) {
		err := serv.addRemoteIPToMetadata(c.Request)
		if err != nil {
			if addrErr, ok := err.(*net.AddrError); ok {
				c.AbortWithError(http.StatusInternalServerError, addrErr).SetType(gin.ErrorTypePrivate)
			} else {
				c.AbortWithError(http.StatusNotAcceptable, err)
			}
			return
		}

		err = serv.processJwt(c.Request)

		if err != nil {
			if isFatalJwtError(err) {
				if jwtValidationErr, ok := err.(*jwt.ValidationError); ok && jwtValidationErr.Inner == jwt.ErrSignatureInvalid {
					c.Error(jwtValidationErr).SetType(gin.ErrorTypePublic)
					c.AbortWithStatusJSON(http.StatusUnauthorized, fmt.Sprintf("Failed to process EXTJWT: %s. Configured secret may be incorrect.", jwtValidationErr))
					return
				}
				c.AbortWithError(http.StatusBadRequest, err).SetType(gin.ErrorTypePublic)
				return
			}
			serv.log.Warn().
				Err(err).
				Msg("Failed to process EXTJWT")
		}

		handler.PostFile(c.Writer, c.Request)
	}
}

func (serv *UploadServer) addRemoteIPToMetadata(req *http.Request) (err error) {
	const uploadMetadataHeader = "Upload-Metadata"
	const remoteIPKey = "RemoteIP"

	metadata := parseMeta(req.Header.Get(uploadMetadataHeader))

	// ensure the client doesn't attempt to specify their own RemoteIP
	for k := range metadata {
		if k == remoteIPKey {
			return fmt.Errorf("Metadata field " + remoteIPKey + " cannot be set by client")
		}
	}

	// determine the originating IP
	remoteIP, err := serv.getDirectOrForwardedRemoteIP(req)
	if err != nil {
		return err
	}

	// add RemoteIP to metadata
	metadata[remoteIPKey] = remoteIP

	// override original header
	req.Header.Set(uploadMetadataHeader, serializeMeta(metadata))

	return
}

// UnknownIssuerError occurs when a file creation request includes an EXTJWT
// with an issuer that is not present in the config
type UnknownIssuerError struct {
	Issuer string
}

func (e UnknownIssuerError) Error() string {
	return fmt.Sprintf("Issuer %#v not configured", e.Issuer)
}

func (serv *UploadServer) getSecretForToken(token *jwt.Token) (interface{}, error) {
	// Don't forget to validate the alg is what you expect:
	if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
		return nil, fmt.Errorf("Unexpected signing method: %v", token.Header["alg"])
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("Failed to get claims")
	}

	issuer, ok := claims["iss"]
	if !ok {
		return nil, fmt.Errorf("Issuer field 'iss' missing from JWT")
	}

	issuerStr, ok := issuer.(string)
	if !ok {
		return nil, fmt.Errorf("Failed to coerce issuer to string")
	}

	secret, ok := serv.cfg.JwtSecretsByIssuer[issuerStr]
	if !ok {
		return nil, &UnknownIssuerError{Issuer: issuerStr}
	}

	return []byte(secret), nil
}

func (serv *UploadServer) processJwt(req *http.Request) (err error) {
	metadata := parseMeta(req.Header.Get("Upload-Metadata"))

	// ensure the client doesn't attempt to specify their own account/issuer fields
	for k := range metadata {
		switch k {
		case "account":
		case "issuer":
			return fmt.Errorf("Metadata field %#v cannot be set by client", k)
		}
	}

	tokenString := metadata["extjwt"]
	if tokenString == "" {
		return nil
	}

	token, err := jwt.Parse(tokenString, serv.getSecretForToken)
	if err != nil {
		return err
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return
	}

	issuer := claims["iss"].(string)
	account, ok := claims["account"].(string)
	if !ok {
		return nil
	}

	metadata["issuer"] = issuer
	metadata["account"] = account

	// override original header
	req.Header.Set("Upload-Metadata", serializeMeta(metadata))

	fmt.Printf("metadata updated: account=%v issuer=%v\n", account, issuer)
	return
}

// ErrInvalidXForwardedFor occurs if the X-Forwarded-For header is trusted but invalid
var ErrInvalidXForwardedFor = errors.New("Failed to parse IP from X-Forwarded-For header")

func (serv *UploadServer) getDirectOrForwardedRemoteIP(req *http.Request) (string, error) {
	// extract direct IP
	remoteIP, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		serv.log.Error().
			Err(err).
			Msg("Could not split address into host and port")
		return "", err
	}

	// use X-Forwarded-For header if direct IP is a trusted reverse proxy
	if forwardedFor := req.Header.Get("X-Forwarded-For"); forwardedFor != "" {
		if serv.remoteIPisTrusted(net.ParseIP(remoteIP)) {
			// We do not check intermediary proxies against the whitelist.
			// If a trusted proxy is appending to and forwarding the value of the
			// header it is receiving, that is an implicit expression of trust
			// which we will honor transitively.

			// take the first comma delimited address
			// this is the original client address
			parts := strings.Split(forwardedFor, ",")
			forwardedForClient := strings.TrimSpace(parts[0])
			forwardedForIP := net.ParseIP(forwardedForClient)
			if forwardedForIP == nil {
				err := ErrInvalidXForwardedFor
				serv.log.Error().
					Err(err).
					Str("client", forwardedForClient).
					Str("remoteIP", remoteIP).
					Msg("Couldn't use trusted X-Forwarded-For header")
				return "", err
			}
			return forwardedForIP.String(), nil
		}
		serv.log.Warn().
			Str("X-Forwarded-For", forwardedFor).
			Str("remoteIP", remoteIP).
			Msg("Untrusted remote attempted to override stored IP")
	}

	// otherwise use direct IP
	return remoteIP, nil
}

func (serv *UploadServer) remoteIPisTrusted(remoteIP net.IP) bool {
	// check if remote IP is a trusted reverse proxy
	for _, trustedNet := range serv.cfg.Server.TrustedReverseProxyRanges {
		if trustedNet.Contains(remoteIP) {
			return true
		}
	}
	return false
}

func (serv *UploadServer) ipRecorder(broadcaster *events.TusEventBroadcaster) {
	channel := broadcaster.Listen()
	for {
		event, ok := <-channel
		if !ok {
			return // channel closed
		}
		if event.Type == hooks.HookPostCreate {
			go func() {
				ip := event.Info.MetaData["RemoteIP"]

				serv.log.Debug().
					Str("id", event.Info.ID).
					Str("ip", ip).
					Msg("Recording uploader IP")

				err := db.UpdateRow(serv.DBConn.DB, `
					UPDATE uploads
					SET uploader_ip = ?
					WHERE id = ?
				`, ip, event.Info.ID)

				if err != nil {
					serv.log.Error().
						Err(err).
						Msg("Failed to record uploader IP")
				}
			}()
		}
	}
}
