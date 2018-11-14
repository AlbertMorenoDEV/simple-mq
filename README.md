Simple MQ
=========

Publish a message
-----------------

```
curl --header "Content-Type: application/json" \
  --request POST \
  --data '{"id":"1","type":"test","data":"hello"}' \
  http://localhost:8001/
```


Get a message
-----------------

```
curl http://localhost:8002/
```

Changelog
---------

### v0.1
* HTTP Publisher with a single message
* HTTP Subscriber

Tools
-----

* End2End package tool: https://github.com/h2non/baloo