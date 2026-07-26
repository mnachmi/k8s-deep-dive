CC      = gcc
CFLAGS  = -Wall -Wextra -Werror
TARGET  = kernel_health_reader

all: $(TARGET)

$(TARGET): kernel_health_reader.c
	$(CC) $(CFLAGS) -o $@ $<

run: $(TARGET)
	./$(TARGET)

clean:
	rm -f $(TARGET)

.PHONY: all run clean
