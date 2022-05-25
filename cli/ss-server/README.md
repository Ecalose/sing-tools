# ss-server

## Requirements

```
* Go 1.18
```

## Install

```shell
git clone https://github.com/SagerNet/sing-tools 
cd sing

cli/ss-server/install.sh

sudo systemctl enable ss
sudo systemctl start ss
```

## Log

```shell
sudo journalctl -u ss --output cat -f
```

## Update

```shell
cli/ss-server/update.sh
```

## Uninstall

```shell
cli/ss-server/uninstall.sh
```