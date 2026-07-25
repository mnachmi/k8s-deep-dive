CC      = gcc
CFLAGS  = -Wall -Wextra -Werror
LDFLAGS = -lpthread
TARGET  = sched_affinity_demo

all: $(TARGET)

$(TARGET): sched_affinity_demo.c
	$(CC) $(CFLAGS) -o $@ $< $(LDFLAGS)

run: $(TARGET)
	./$(TARGET)

clean:
	rm -f $(TARGET)

.PHONY: all run clean
