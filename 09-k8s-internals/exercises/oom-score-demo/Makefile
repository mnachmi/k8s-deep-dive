CC      = gcc
CFLAGS  = -Wall -Wextra -Werror
TARGET  = oom_score_demo

all: $(TARGET)

$(TARGET): oom_score_demo.c
	$(CC) $(CFLAGS) -o $@ $<

run: $(TARGET)
	sudo ./$(TARGET)

clean:
	rm -f $(TARGET)

.PHONY: all run clean
