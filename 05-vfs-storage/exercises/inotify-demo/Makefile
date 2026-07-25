CC      := gcc
CFLAGS  := -Wall -Wextra -Werror
TARGET  := inotify_demo
SRC     := inotify_demo.c

.PHONY: all run clean

all: $(TARGET)

$(TARGET): $(SRC)
	$(CC) $(CFLAGS) -o $@ $<

run: all
	./$(TARGET)

clean:
	rm -f $(TARGET)
