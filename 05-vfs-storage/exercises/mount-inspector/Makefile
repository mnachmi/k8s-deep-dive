.PHONY: build run run-pid clean

TARGET = mount-inspector

build:
	go build -o $(TARGET) .

run: build
	./$(TARGET)

run-pid: build
	./$(TARGET) --pid $(PID)

clean:
	rm -f $(TARGET)
