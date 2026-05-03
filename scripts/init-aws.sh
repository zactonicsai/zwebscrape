#!/bin/bash
echo "Creating S3 bucket: scraper-data"
awslocal s3 mb s3://scraper-data
awslocal s3api put-bucket-cors --bucket scraper-data --cors-configuration '{
  "CORSRules": [{
    "AllowedHeaders": ["*"],
    "AllowedMethods": ["GET", "PUT", "POST"],
    "AllowedOrigins": ["*"]
  }]
}'
echo "Bucket created successfully"
