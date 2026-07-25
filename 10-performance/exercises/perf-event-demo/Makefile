CC      = gcc
CFLAGS  = -Wall -Wextra -Werror
TARGET  = perf_event_demo

all: $(TARGET)

$(TARGET): perf_event_demo.c
	$(CC) $(CFLAGS) -o $@ $<

run: $(TARGET)
	./$(TARGET)

clean:
	rm -f $(TARGET)

.PHONY: all run clean
