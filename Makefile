.PHONY: $(MAKECMDGOALS)

build: cron

mkbin:
	@mkdir -p bin/

cron: mkbin
	go build -o bin/marketwatch-cron ./cron
