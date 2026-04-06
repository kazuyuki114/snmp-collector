#!/bin/bash

echo "Waiting for Kafka to be ready"
# Wait for Kafka to be available
until kafka-topics --bootstrap-server kafka:9092 --list &>/dev/null; do
  echo "Waiting for Kafka to start"
  sleep 5
done

echo "Kafka is ready! Deleting all topics"

# Get all topics and delete them
for topic in $(kafka-topics --bootstrap-server kafka:9092 --list); do
  echo "Deleting topic: $topic"
  kafka-topics --bootstrap-server kafka:9092 --delete --topic "$topic"
done

echo "All topics deleted!"

echo "Creating topics..."
kafka-topics --bootstrap-server kafka:9092 \
  --create \
  --topic snmp \
  --partitions 2 \
  --replication-factor 1


echo "Topics created successfully!"

echo "Final Topic Details:"
kafka-topics --bootstrap-server kafka:9092 --list

# Create a signal file to indicate topics are ready
touch /tmp/kafka-topics-ready
echo "Kafka topics initialization complete!"