CC      := gcc
CFLAGS  := -Wall -Wextra -Werror
TARGET  := tcp_echo_demo
SRC     := tcp_echo_demo.c

.PHONY: all run clean

all: $(TARGET)

$(TARGET): $(SRC)
	$(CC) $(CFLAGS) -o $@ $<

run: all
	./$(TARGET)

clean:
	rm -f $(TARGET)
