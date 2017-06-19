# gonigmo is a Go wrapper for the Onigmo regex engine #

Based on [github.com/moovweb/rubex](https://github.com/moovweb/rubex) by Zhigang Chen

## Installation ##

Install Onigmo:

```
git clone https://github.com/k-takata/Onigmo.git --depth=1
cd Onigmo
./configure && make && sudo make install
```

Install or update gonigmo:

```
go get -u -f github.com/ungerik/gonigmo
```

You may have to disable cgocheck to prevent false positives:

    GODEBUG=cgocheck=0


## Example Usage ##

```go
    import "github.com/ungerik/gonigmo"
    
    rxp := gonigmo.MustCompile("[a-z]*")
    if err != nil {
        // whoops
    }
    result := rxp.FindString("a me my")
    if result != "" {
        // FOUND A STRING!! YAY! Must be "a" in this instance
    } else {
        // no good
    }
```
