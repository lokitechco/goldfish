package main

import (
	"flag"
	"fmt"
	"log"
	"time"
	"os"
	"os/signal"
	"syscall"

	"github.com/caiyeon/goldfish/config"
	"github.com/caiyeon/goldfish/handlers"
	"github.com/caiyeon/goldfish/vault"
	"github.com/gorilla/csrf"
	"github.com/gorilla/securecookie"
	"github.com/labstack/echo"
	"github.com/labstack/echo/middleware"

	"golang.org/x/crypto/acme/autocert"
)

var (
	devMode       bool
	wrappingToken string
	cfgPath       string
	cfg           *config.Config
	devVaultCh    chan struct{}
	err           error
	printVersion  bool
)

func init() {
	flag.BoolVar(&devMode, "dev", false, "Set to true to save time in development. DO NOT SET TO TRUE IN PRODUCTION!!")
	flag.BoolVar(&printVersion, "version", false, "Display goldfish's version and exit")
	flag.StringVar(&wrappingToken, "token", "", "Token generated from approle (must be wrapped!)")
	flag.StringVar(&cfgPath, "config", "", "The path of the deployment config HCL file")

	// if vault dev core is active, relay shutdown signal
	shutdownCh := make(chan os.Signal, 4)
	signal.Notify(shutdownCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<- shutdownCh
		log.Println("\n\n==> Goldfish shutdown triggered")
		if devVaultCh != nil { close(devVaultCh) }
		time.Sleep(time.Second)
		os.Exit(0)
	}()
}

func main() {
	// if --version, print and exit success
	flag.Parse()
	if printVersion {
		log.Println(versionString)
		os.Exit(0)
	}

	// if dev mode, run a localhost dev vault instance
	if devMode {
		cfg, devVaultCh, wrappingToken, err = config.LoadConfigDev()
	} else {
		cfg, err = config.LoadConfigFile(cfgPath)
	}
	if err != nil {
		panic(err)
	}

	// if API wrapper can't start, panic is justified
	vault.VaultAddress = cfg.Vault.Address
	vault.VaultSkipTLS = cfg.Vault.Tls_skip_verify
	if err := vault.StartGoldfishWrapper(
		wrappingToken,
		cfg.Vault.Approle_login,
		cfg.Vault.Approle_id,
	); err != nil {
		panic(err)
	}

	// load config from vault and start goroutines
	if err := vault.LoadRuntimeConfig(cfg.Vault.Runtime_config); err != nil {
		panic(err)
	}

	// if we got here, goldfish has hooked up to vault successfully
	if devMode {
		fmt.Printf(devInitString)
	}
	fmt.Printf(versionString + initString)

	// instantiate echo web server
	e := echo.New()
	e.HideBanner = true

	// setup middleware
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(echo.WrapMiddleware(
		csrf.Protect(
			// Generate a new encryption key for cookies each launch
			// invalidating previous goldfish instance's cookies is purposeful
			[]byte(securecookie.GenerateRandomKey(32)),
			// https-only unless tls_disable
			csrf.Secure(!cfg.Listener.Tls_disable),
		)))

	// unless explicitly disabled, some extra https configurations need to be set
	if !cfg.Listener.Tls_disable {
		// add extra security headers
		e.Use(middleware.SecureWithConfig(middleware.SecureConfig{
			XSSProtection:         "1; mode=block",
			ContentTypeNosniff:    "nosniff",
			XFrameOptions:         "SAMEORIGIN",
			ContentSecurityPolicy: "ddefault-src 'self' blob: 'unsafe-inline' buttons.github.io api.github.com;",
		}))

		// if redirect is set, forward port 80 to port 443
		if cfg.Listener.Tls_autoredirect {
			e.Pre(middleware.HTTPSRedirect())
			go func(c *echo.Echo){
				e.Logger.Fatal(e.Start(":80"))
			}(e)
		}

		// if cert file and key file are not provided, try using let's encrypt
		if cfg.Listener.Tls_cert_file == "" && cfg.Listener.Tls_key_file == "" {
			e.AutoTLSManager.Cache = autocert.DirCache("/var/www/.cache")
			e.AutoTLSManager.HostPolicy = autocert.HostWhitelist(cfg.Listener.Address)
			e.Use(middleware.HTTPSRedirectWithConfig(middleware.RedirectConfig{
				Code: 301,
			}))
		}
	}

	// static routing of webpack'd folder
	e.Static("/", "public")

	// API routing
	e.GET("/api/health", handlers.VaultHealth())

	e.GET("/api/login/csrf", handlers.FetchCSRF())
	e.POST("/api/login", handlers.Login())
	e.POST("/api/login/renew-self", handlers.RenewSelf())

	e.GET("/api/users", handlers.GetUsers())
	e.GET("/api/users/csrf", handlers.FetchCSRF())
	e.GET("/api/tokencount", handlers.GetTokenCount())
	e.GET("/api/users/role", handlers.GetRole())
	e.GET("/api/users/listroles", handlers.ListRoles())
	e.POST("/api/users/revoke", handlers.DeleteUser())
	e.POST("/api/users/create", handlers.CreateUser())

	e.GET("/api/policy", handlers.GetPolicy())
	e.DELETE("/api/policy", handlers.DeletePolicy())

	e.GET("/api/policy/request", handlers.GetPolicyRequest())
	e.POST("/api/policy/request", handlers.AddPolicyRequest())
	e.POST("/api/policy/request/update", handlers.UpdatePolicyRequest())
	e.DELETE("/api/policy/request/:id", handlers.DeletePolicyRequest())

	e.GET("/api/transit", handlers.TransitInfo())
	e.POST("/api/transit/encrypt", handlers.EncryptString())
	e.POST("/api/transit/decrypt", handlers.DecryptString())

	e.GET("/api/mounts", handlers.GetMounts())
	e.GET("/api/mounts/:mountname", handlers.GetMount())
	e.POST("/api/mounts/:mountname", handlers.ConfigMount())

	e.GET("/api/secrets", handlers.GetSecrets())
	e.POST("/api/secrets", handlers.PostSecrets())
	e.DELETE("/api/secrets", handlers.DeleteSecrets())

	e.GET("/api/bulletins", handlers.GetBulletins())

	e.GET("/api/wrapping", handlers.FetchCSRF())
	e.POST("/api/wrapping/wrap", handlers.WrapHandler())
	e.POST("/api/wrapping/unwrap", handlers.UnwrapHandler())

	// serving both static folder and API
	if (cfg.Listener.Tls_disable) {
		// launch http-only listener
		e.Logger.Fatal(e.Start(cfg.Listener.Address))
	} else if cfg.Listener.Tls_cert_file == "" && cfg.Listener.Tls_key_file == "" {
		// if https is enabled, but no cert provided, try let's encrypt
		e.Logger.Fatal(e.StartAutoTLS(":443"))
	} else {
		// launch listener in https
		e.Logger.Fatal(e.StartTLS(
			cfg.Listener.Address,
			cfg.Listener.Tls_cert_file,
			cfg.Listener.Tls_key_file,
		))
	}
}

const versionString = "Goldfish version: v0.4.1"

const devInitString = `

---------------------------------------------------
Starting local vault dev instance...
Your unseal token and root token can be found above
`

const initString = `
Goldfish successfully bootstrapped to vault

  .
  ...             ...
  .........       ......
   ...........   ..........
     .......... ...............
     .............................
      .............................
         ...........................
        ...........................
        ..........................
        ...... ..................
      ......    ...............
     ..        ..      ....
    .                 ..


`
