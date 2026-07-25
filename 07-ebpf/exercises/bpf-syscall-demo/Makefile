CC      := gcc
CFLAGS  := -Wall -Wextra -Werror
TARGET  := bpf_syscall_demo
SRC     := bpf_syscall_demo.c

.PHONY: all build run clean

all: build

build: $(TARGET)

$(TARGET): $(SRC)
	$(CC) $(CFLAGS) -o $@ $<

run: build
	sudo ./$(TARGET)

clean:
	rm -f $(TARGET)
