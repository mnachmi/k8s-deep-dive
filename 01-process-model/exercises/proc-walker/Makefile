.PHONY: build run clean

TARGET = proc-walker

build:
	go build -o $(TARGET) .

run: build
	./$(TARGET)

# Pass a PID to also show its namespace symlinks
run-pid: build
	./$(TARGET) $(PID)

clean:
	rm -f $(TARGET)
