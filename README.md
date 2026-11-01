# shapebundle

Loads traffic shaping bundles for [zmux](https://github.com/zmuxio/zmux-go)
sessions: parse and validate, optional signature check, fetch and cache,
and turn a bundle into the shaper factory, session limits and pool policy
zmux takes.

```sh
go get github.com/djylb/shapebundle
```

```go
f, _ := shapebundle.NewFetcher(shapebundle.FetcherConfig{URL: url, CacheDir: dir})
f.Load()
go f.Run(ctx)

p, _ := shapebundle.NewProfile(shapebundle.ProfileOptions{}, f.Current())
cfg.Shaper = p.Factory()
```

See the package documentation for details.

## License

Apache-2.0
