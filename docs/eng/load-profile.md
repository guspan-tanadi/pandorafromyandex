[Home](../index.md)

---

# Load profile

To determine what load to create on the server, use a load profile. It sets how the load will be changed and maintained.

Most tools have their own formats for defining load profiles. When configuring a test via the UI, you can get the final profile by adding sections in the correct order.


## line

Linearly increases the load in a given range over a certain period of time.

Example:

```
{duration: 180s, type: line, from: 1, to: 10000} # the load increases from 1 to 10000 requests per second over 180 seconds
```

## const

Maintains the specified load for a certain time.

Example:

```
{duration: 300s, type: const, ops: 10000} # generates 10000 requests per second for 300 seconds
```

## step

Increases the load with the specified increment size from one value to another for a certain time.

Example:

```
{duration: 30s, type: step, from: 10, to: 100, step: 5} # the load increases from 10 to 100 requests per second in increments of 5 and with a step duration of 30 seconds
```

## once

Sends the specified number of requests once and completes the test. There are no restrictions on the number of requests.

Example:

```
{type: once, times: 133} # sends 133 requests at the start of this test section and completes the test
```

## unlimited

Sends as many requests as the target can accept within the established connections without restrictions during the specified time.

Example:

```
{type: unlimited, duration: 30s} # unlimited load for 30 seconds
```

---

[Home](../index.md)
